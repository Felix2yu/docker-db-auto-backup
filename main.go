package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

func main() {
	cfg := loadConfig()
	initLogger(cfg)

	// 压缩算法写错会让每一个容器的备份都失败，却在日志里表现得像备份本身出了问题，
	// 因此启动时直接拒绝（Kopia 启用时压缩被强制为 plain，不会误伤）。
	if algo := cfg.effectiveCompression(); !isValidCompression(algo) {
		logError("不支持的压缩方式，请检查 COMPRESSION", "value", algo, "可选", "gzip / lzma / xz / bz2 / plain")
		os.Exit(1)
	}

	initMetrics()
	startMetricsServer(cfg.metricsAddr)

	if err := loadExtraProviders(cfg.patternsFile); err != nil {
		logError("加载镜像识别配置失败，将只使用内置规则", "error", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// B1：提供子命令，便于手动触发、查看与离线校验备份。
	if len(os.Args) > 1 {
		os.Exit(runCLI(ctx, cfg, os.Args[1:]))
	}

	dc, err := newDockerClient(ctx)
	if err != nil {
		logError("连接 Docker 失败", "error", err)
		os.Exit(1)
	}

	if cfg.schedule != "" {
		logInfo("按计划运行备份", "schedule", cfg.schedule, "timezone", cfg.location().String())
		runScheduled(ctx, cfg, dc)
		return
	}

	if err := backup(ctx, cfg, dc, cfg.nowIn()); err != nil {
		os.Exit(1)
	}
}

// runScheduled 按 cron 表达式循环执行。
//
// C1：单轮备份失败不再终止进程——否则容器会因 restart 策略重启，
// 但本次窗口已错过，且持续失败时会陷入 crash-restart 循环。
func runScheduled(ctx context.Context, cfg *config, dc *dockerClient) {
	schedule, err := cron.ParseStandard(cfg.schedule)
	if err != nil {
		logError("无效的备份计划，调度已停止", "schedule", cfg.schedule, "error", err)
		return
	}

	for {
		next := schedule.Next(cfg.nowIn())
		logInfo("等待下一次备份", "at", next.Format("2006-01-02 15:04:05 MST"))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			logInfo("收到退出信号，调度已停止")
			return
		case <-timer.C:
		}

		if err := backup(ctx, cfg, dc, cfg.nowIn()); err != nil {
			logError("本轮备份失败，将在下一个周期重试", "error", err)
			continue
		}
	}
}
