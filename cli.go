package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// 子命令（B1）：除默认调度模式外，提供手动执行、列表、离线校验与状态查看。

const usageText = `用法: db-auto-backup [子命令] [选项]

子命令:
  run                 立即执行一次备份（无需修改 SCHEDULE）
  list [--date 日期]  列出备份；指定日期时显示文件明细
  verify [--date 日期] 离线重跑校验并比对清单中的 SHA256（默认最新日期）
  status              显示最近一次备份的运行状态与下次计划时间
  help                显示本帮助

不带子命令时按 SCHEDULE 调度运行；SCHEDULE 为空则立即执行一次后退出。`

func runCLI(ctx context.Context, cfg *config, args []string) int {
	if len(args) == 0 {
		fmt.Println(usageText)
		return 0
	}
	switch args[0] {
	case "run":
		return cmdRun(ctx, cfg)
	case "list":
		return cmdList(cfg, flagValue(args, "date"))
	case "verify":
		return cmdVerify(cfg, flagValue(args, "date"))
	case "status":
		return cmdStatus(cfg)
	case "help", "-h", "--help":
		fmt.Println(usageText)
		return 0
	default:
		logError("未知子命令", "cmd", args[0])
		fmt.Println(usageText)
		return 2
	}
}

// flagValue 支持 "--date=2026-01-01" 与 "--date 2026-01-01" 两种写法。
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == "--"+name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "--"+name+"=") {
			return strings.TrimPrefix(a, "--"+name+"=")
		}
	}
	return ""
}

func cmdRun(ctx context.Context, cfg *config) int {
	dc, err := newDockerClient(ctx)
	if err != nil {
		logError("连接 Docker 失败", "error", err)
		return 1
	}
	logInfo("手动执行一次备份")
	if err := backup(ctx, cfg, dc, cfg.nowIn()); err != nil {
		logError("备份失败", "error", err)
		return 1
	}
	return 0
}

func cmdList(cfg *config, date string) int {
	if date != "" {
		dir := filepath.Join(cfg.backupDir, date)
		m, err := readManifest(dir)
		if err != nil {
			logError("读取备份清单失败", "date", date, "error", err)
			return 1
		}
		fmt.Printf("日期: %s  状态: %s  容器: %d  文件: %d  体积: %s\n",
			m.Date, statusLabel(m.Status), len(m.Containers), m.fileCount(), humanBytes(m.totalBytes()))
		for _, c := range m.Containers {
			fmt.Printf("\n%s (%s, %s)\n", c.Name, c.Provider, modeLabel(c.Mode))
			for _, f := range c.Files {
				sum := f.SHA256
				if len(sum) > 16 {
					sum = sum[:16]
				}
				fmt.Printf("  %-40s %10s  %s\n", f.Path, humanBytes(f.Size), sum)
			}
		}
		return 0
	}

	dates, err := listBackupDates(cfg.backupDir)
	if err != nil {
		logError("读取备份目录失败", "dir", cfg.backupDir, "error", err)
		return 1
	}
	if len(dates) == 0 {
		fmt.Println("尚无备份记录")
		return 0
	}
	fmt.Printf("%-12s %-10s %8s %8s %12s\n", "日期", "状态", "容器", "文件", "体积")
	for _, d := range dates {
		m, err := readManifest(filepath.Join(cfg.backupDir, d))
		if err != nil {
			fmt.Printf("%-12s %-10s\n", d, "无清单")
			continue
		}
		fmt.Printf("%-12s %-10s %8d %8d %12s\n",
			d, statusLabel(m.Status), len(m.Containers), m.fileCount(), humanBytes(m.totalBytes()))
	}
	return 0
}

func cmdVerify(cfg *config, date string) int {
	if date == "" {
		dates, err := listBackupDates(cfg.backupDir)
		if err != nil || len(dates) == 0 {
			logError("没有可校验的备份", "error", err)
			return 1
		}
		date = dates[len(dates)-1]
	}
	dir := filepath.Join(cfg.backupDir, date)
	m, err := readManifest(dir)
	if err != nil {
		logError("读取备份清单失败", "date", date, "error", err)
		return 1
	}

	fmt.Printf("校验 %s（共 %d 个文件）\n", date, m.fileCount())
	failed := 0
	for _, c := range m.Containers {
		for _, f := range c.Files {
			path := filepath.Join(cfg.backupDir, f.Path)
			problems := verifyOneFile(cfg, path, f)
			if len(problems) == 0 {
				fmt.Printf("  OK   %s\n", f.Path)
				continue
			}
			failed++
			for _, p := range problems {
				fmt.Printf("  FAIL %s: %s\n", f.Path, p)
			}
		}
	}
	if failed > 0 {
		fmt.Printf("\n%d 个文件校验失败\n", failed)
		return 1
	}
	fmt.Println("\n全部文件校验通过")
	return 0
}

func verifyOneFile(cfg *config, path string, f fileEntry) []string {
	var problems []string
	fi, err := os.Stat(path)
	if err != nil {
		return []string{fmt.Sprintf("文件不可访问: %v", err)}
	}
	if fi.Size() != f.Size {
		problems = append(problems, fmt.Sprintf("体积与清单不一致（清单 %s，实际 %s）",
			humanBytes(f.Size), humanBytes(fi.Size())))
	}
	if f.SHA256 != "" {
		sum, err := fileChecksum(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("计算校验和失败: %v", err))
		} else if sum != f.SHA256 {
			problems = append(problems, "SHA256 与清单不一致（文件已被修改或损坏）")
		}
	}
	if err := validateBackupFile(cfg, path, fileExtFromName(path)); err != nil {
		problems = append(problems, fmt.Sprintf("内容校验失败: %v", err))
	}
	return problems
}

func fileExtFromName(path string) string {
	name := filepath.Base(path)
	for _, suffix := range []string{".gz", ".xz", ".bz2"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return strings.TrimPrefix(filepath.Ext(name), ".")
}

func cmdStatus(cfg *config) int {
	dates, err := listBackupDates(cfg.backupDir)
	if err != nil {
		logError("读取备份目录失败", "dir", cfg.backupDir, "error", err)
		return 1
	}
	if len(dates) == 0 {
		fmt.Println("尚无备份记录")
		return 0
	}
	latest := dates[len(dates)-1]
	m, err := readManifest(filepath.Join(cfg.backupDir, latest))
	if err != nil {
		logError("读取备份清单失败", "date", latest, "error", err)
		return 1
	}

	fmt.Printf("备份目录:      %s\n", cfg.backupDir)
	fmt.Printf("时区:          %s\n", cfg.location().String())
	fmt.Printf("最近备份日期:  %s\n", latest)
	fmt.Printf("状态:          %s\n", statusLabel(m.Status))
	fmt.Printf("运行时间:      %s（耗时 %.1f 秒）\n", m.RunAt.Format(time.RFC3339), m.DurationSeconds)
	fmt.Printf("容器 / 文件:   %d / %d\n", len(m.Containers), m.fileCount())
	fmt.Printf("总体积:        %s\n", humanBytes(m.totalBytes()))
	if m.Kopia != nil {
		if m.Kopia.Pushed {
			fmt.Printf("异地快照:      已推送（%s）\n", orDefault(m.Kopia.SnapshotID, "无 ID"))
		} else {
			fmt.Printf("异地快照:      失败（%s）\n", orDefault(m.Kopia.Error, "未知原因"))
		}
	}
	if len(m.Failures) > 0 {
		fmt.Println("失败明细:")
		for _, f := range m.Failures {
			fmt.Printf("  - %s\n", f)
		}
	}
	if len(m.Warnings) > 0 {
		fmt.Println("警告:")
		for _, w := range m.Warnings {
			fmt.Printf("  - %s\n", w)
		}
	}

	if cfg.schedule != "" {
		if schedule, err := cron.ParseStandard(cfg.schedule); err == nil {
			fmt.Printf("调度:          %s（下次 %s）\n", cfg.schedule,
				schedule.Next(cfg.nowIn()).Format(time.RFC3339))
		} else {
			fmt.Printf("调度:          %s（解析失败）\n", cfg.schedule)
		}
	} else {
		fmt.Println("调度:          未设置（单次模式）")
	}
	if m.Status != statusSuccess {
		return 1
	}
	return 0
}

func modeLabel(mode string) string {
	switch mode {
	case modeSingle:
		return "单库"
	case modeFullFallback:
		return "全库（单库回退）"
	case modeFull:
		return "全库"
	}
	return mode
}
