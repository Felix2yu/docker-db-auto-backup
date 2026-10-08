package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureStdout 捕获一次调用打印到标准输出的内容（子命令都是直接 fmt.Printf）。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()

	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	return out
}

// writeDateManifest 在备份目录下造一个日期目录及其清单，返回该目录。
func writeDateManifest(t *testing.T, backupDir, date string, m *backupManifest) string {
	t.Helper()
	dir := filepath.Join(backupDir, date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const validSQLDump = "-- MySQL dump 10.13\nCREATE TABLE t (id int);\n-- Dump completed on 2026-09-21 03:00:00\n"

func cliTestConfig(t *testing.T) *config {
	t.Helper()
	return &config{backupDir: t.TempDir(), compression: "plain", checksumEnabled: true}
}

func TestFlagValue(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"list", "--date", "2026-01-02"}, "2026-01-02"},
		{[]string{"list", "--date=2026-01-03"}, "2026-01-03"},
		{[]string{"list"}, ""},
		{[]string{"list", "--date"}, ""},
		{[]string{"list", "--other", "x"}, ""},
	}
	for _, c := range cases {
		if got := flagValue(c.args, "date"); got != c.want {
			t.Errorf("flagValue(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestModeLabel(t *testing.T) {
	for mode, want := range map[string]string{
		modeSingle:       "单库",
		modeFull:         "全库",
		modeFullFallback: "全库（单库回退）",
		"other":          "other",
		"":               "",
	} {
		if got := modeLabel(mode); got != want {
			t.Errorf("modeLabel(%q) = %q, want %q", mode, got, want)
		}
	}
}

func TestRunCLIUnknownAndHelp(t *testing.T) {
	cfg := cliTestConfig(t)

	captureStdout(t, func() {
		if code := runCLI(context.Background(), cfg, nil); code != 0 {
			t.Errorf("空参数应打印用法并返回 0, got %d", code)
		}
	})
	captureStdout(t, func() {
		if code := runCLI(context.Background(), cfg, []string{"help", "-h", "--help"}); code != 0 {
			t.Errorf("help 应返回 0, got %d", code)
		}
	})
	out := captureStdout(t, func() {
		if code := runCLI(context.Background(), cfg, []string{"nope"}); code != 2 {
			t.Errorf("未知子命令应返回 2, got %d", code)
		}
	})
	if !strings.Contains(out, "用法:") {
		t.Errorf("未知子命令也应打印用法, got %q", out)
	}
}

// cmdRun 不能真的连上 Docker：把 DOCKER_HOST 指向不存在的 socket，只验证失败路径。
// 失败原因走的是 slog（直接写真实 stdout），所以这里只断言退出码。
func TestCmdRunWithoutDocker(t *testing.T) {
	cfg := cliTestConfig(t)
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "nonexistent.sock"))

	captureStdout(t, func() {
		if code := runCLI(context.Background(), cfg, []string{"run"}); code != 1 {
			t.Errorf("连不上 Docker 时 run 应返回 1, got %d", code)
		}
	})
}

func TestCmdRunInvalidDockerHost(t *testing.T) {
	cfg := cliTestConfig(t)
	t.Setenv("DOCKER_HOST", "无法解析的 host")

	captureStdout(t, func() {
		if code := runCLI(context.Background(), cfg, []string{"run"}); code != 1 {
			t.Errorf("非法 DOCKER_HOST 应返回 1, got %d", code)
		}
	})
}

func TestCmdListDates(t *testing.T) {
	cfg := cliTestConfig(t)

	out := captureStdout(t, func() {
		if code := cmdList(cfg, ""); code != 0 {
			t.Errorf("空目录应返回 0, got %d", code)
		}
	})
	if !strings.Contains(out, "尚无备份记录") {
		t.Errorf("got %q", out)
	}

	m := &backupManifest{Version: manifestVersion, Date: "2026-09-21", Status: statusSuccess,
		Containers: []containerManifest{{Name: "pg", Provider: "postgres", Mode: modeSingle,
			Files: []fileEntry{{Path: "2026-09-21/pg/appdb.sql", Size: 1234, Database: "appdb"}}}}}
	writeDateManifest(t, cfg.backupDir, "2026-09-21", m)
	// 没有清单的日期目录也要能列出来
	if err := os.MkdirAll(filepath.Join(cfg.backupDir, "2026-09-22"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 非日期目录应被忽略
	if err := os.MkdirAll(filepath.Join(cfg.backupDir, ".tmp"), 0o755); err != nil {
		t.Fatal(err)
	}

	out = captureStdout(t, func() {
		if code := cmdList(cfg, ""); code != 0 {
			t.Errorf("got %d", code)
		}
	})
	if !strings.Contains(out, "2026-09-21") || !strings.Contains(out, "无清单") {
		t.Errorf("列表应同时包含有清单与无清单的日期:\n%s", out)
	}
	if strings.Contains(out, ".tmp") {
		t.Errorf("非日期目录不应出现在列表里:\n%s", out)
	}
}

func TestCmdListDateDetail(t *testing.T) {
	cfg := cliTestConfig(t)
	m := &backupManifest{Version: manifestVersion, Date: "2026-09-21", Status: statusPartial,
		Containers: []containerManifest{
			{Name: "pg", Provider: "postgres", Mode: modeSingle, Files: []fileEntry{
				{Path: "2026-09-21/pg/appdb.sql", Size: 4096, Database: "appdb", SHA256: strings.Repeat("a", 64)},
				{Path: "2026-09-21/pg/system/globals.sql", Size: 512, Database: "globals", System: true},
			}},
			{Name: "rd", Provider: "redis", Mode: modeFull, Files: []fileEntry{
				{Path: "2026-09-21/rd.rdb", Size: 200},
			}},
		}}
	writeDateManifest(t, cfg.backupDir, "2026-09-21", m)

	out := captureStdout(t, func() {
		if code := cmdList(cfg, "2026-09-21"); code != 0 {
			t.Errorf("got %d", code)
		}
	})
	for _, want := range []string{"appdb.sql", "单库", "全库", strings.Repeat("a", 16), "globals"} {
		if !strings.Contains(out, want) {
			t.Errorf("明细应包含 %q:\n%s", want, out)
		}
	}

	captureStdout(t, func() {
		if c := cmdList(cfg, "2026-01-01"); c != 1 {
			t.Errorf("不存在的日期应返回 1, got %d", c)
		}
	})
}

func TestCmdVerify(t *testing.T) {
	cfg := cliTestConfig(t)
	dir := filepath.Join(cfg.backupDir, "2026-09-21")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "pg", "appdb.sql")
	if err := os.MkdirAll(filepath.Dir(good), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, []byte(validSQLDump), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(good)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := fileChecksum(good)
	if err != nil {
		t.Fatal(err)
	}

	m := &backupManifest{Version: manifestVersion, Date: "2026-09-21", Status: statusSuccess,
		Containers: []containerManifest{{Name: "pg", Provider: "postgres", Mode: modeSingle, Files: []fileEntry{
			{Path: "2026-09-21/pg/appdb.sql", Size: fi.Size(), SHA256: sum, Database: "appdb"},
		}}}}
	writeDateManifest(t, cfg.backupDir, "2026-09-21", m)

	var code int
	out := captureStdout(t, func() { code = cmdVerify(cfg, "") })
	if code != 0 {
		t.Errorf("完好备份应返回 0, got %d", code)
	}
	if !strings.Contains(out, "全部文件校验通过") {
		t.Errorf("got %q", out)
	}

	// 1) 文件被改动：SHA 与体积都对不上
	if err := os.WriteFile(good, []byte(validSQLDump+"x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		if code := cmdVerify(cfg, "2026-09-21"); code != 1 {
			t.Errorf("内容变化应返回 1, got %d", code)
		}
	})
	if !strings.Contains(out, "SHA256 与清单不一致") || !strings.Contains(out, "体积与清单不一致") {
		t.Errorf("应同时报告体积与校验和不一致:\n%s", out)
	}

	// 2) 文件丢失
	if err := os.Remove(good); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		if code := cmdVerify(cfg, "2026-09-21"); code != 1 {
			t.Errorf("文件丢失应返回 1, got %d", code)
		}
	})
	if !strings.Contains(out, "文件不可访问") {
		t.Errorf("got %q", out)
	}
}

func TestCmdVerifyNoBackup(t *testing.T) {
	cfg := cliTestConfig(t)
	var code int
	captureStdout(t, func() { code = cmdVerify(cfg, "") })
	if code != 1 {
		t.Errorf("没有可校验的备份应返回 1, got %d", code)
	}
}

// 截断的 dump 即便体积与清单一致，也必须被内容结构校验拦下。
func TestVerifyOneFileDetectsTruncatedDump(t *testing.T) {
	cfg := cliTestConfig(t)
	path := filepath.Join(t.TempDir(), "truncated.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE t (id int);\n-- 被截断"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	problems := verifyOneFile(cfg, path, fileEntry{Size: fi.Size()})
	if len(problems) == 0 || !strings.Contains(problems[0], "内容校验失败") {
		t.Errorf("缺少完成标记应报内容校验失败, got %v", problems)
	}
}

func TestCmdStatus(t *testing.T) {
	kopiaID := "k1a2b3c4"
	cfg := cliTestConfig(t)
	cfg.schedule = "0 0 * * *"
	m := &backupManifest{Version: manifestVersion, Date: "2026-09-21", RunAt: time.Now(),
		Status: statusSuccess, DurationSeconds: 12.4,
		Kopia:      &kopiaManifestInfo{Pushed: true, SnapshotID: kopiaID},
		Containers: []containerManifest{{Name: "pg", Provider: "postgres", Mode: modeSingle, Files: []fileEntry{{Path: "a", Size: 10}}}}}
	writeDateManifest(t, cfg.backupDir, "2026-09-21", m)

	out := captureStdout(t, func() {
		if code := cmdStatus(cfg); code != 0 {
			t.Errorf("成功状态应返回 0, got %d", code)
		}
	})
	for _, want := range []string{"备份目录:", kopiaID, "已推送", "调度:"} {
		if !strings.Contains(out, want) {
			t.Errorf("状态输出应包含 %q:\n%s", want, out)
		}
	}

	// 异地推送失败：显示原因
	m.Kopia = &kopiaManifestInfo{Pushed: false, Error: "repository unreachable"}
	m.Status = statusPartial
	m.Failures = []string{"rd: BGSAVE 超时未完成"}
	m.Warnings = []string{"容器 pg 备份体积下降"}
	writeDateManifest(t, cfg.backupDir, "2026-09-21", m)
	out = captureStdout(t, func() {
		if code := cmdStatus(cfg); code != 1 {
			t.Errorf("部分失败应返回 1, got %d", code)
		}
	})
	for _, want := range []string{"repository unreachable", "失败明细", "警告", "BGSAVE"} {
		if !strings.Contains(out, want) {
			t.Errorf("状态输出应包含 %q:\n%s", want, out)
		}
	}

	// 无调度与坏调度
	cfg.schedule = ""
	out = captureStdout(t, func() {
		_ = cmdStatus(cfg)
	})
	if !strings.Contains(out, "未设置（单次模式）") {
		t.Errorf("got %q", out)
	}
	cfg.schedule = "not a cron"
	out = captureStdout(t, func() {
		_ = cmdStatus(cfg)
	})
	if !strings.Contains(out, "解析失败") {
		t.Errorf("got %q", out)
	}
}

func TestCmdStatusEmptyAndBroken(t *testing.T) {
	cfg := cliTestConfig(t)
	out := captureStdout(t, func() {
		if code := cmdStatus(cfg); code != 0 {
			t.Errorf("没有备份应返回 0, got %d", code)
		}
	})
	if !strings.Contains(out, "尚无备份记录") {
		t.Errorf("got %q", out)
	}

	dir := filepath.Join(cfg.backupDir, "2026-09-21")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), []byte("{ 坏 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		if code := cmdStatus(cfg); code != 1 {
			t.Errorf("清单损坏应返回 1, got %d", code)
		}
	})
	captureStdout(t, func() {
		if code := cmdList(cfg, "2026-09-21"); code != 1 {
			t.Errorf("读取坏清单的明细应返回 1, got %d", code)
		}
	})
}

// 子命令分发本身也要走一遍，避免 switch 分支与实现脱节。
func TestRunCLIDispatchesSubcommands(t *testing.T) {
	cfg := cliTestConfig(t)
	ctx := context.Background()

	for _, args := range [][]string{{"list"}, {"verify"}, {"status"}} {
		captureStdout(t, func() {
			if code := runCLI(ctx, cfg, args); code != 1 && code != 0 {
				t.Errorf("%v 返回了未预期的退出码 %d", args, code)
			}
		})
	}
	// -h/--help 与 help 等价
	captureStdout(t, func() {
		if code := runCLI(ctx, cfg, []string{"-h"}); code != 0 {
			t.Errorf("got %d", code)
		}
	})
}

// 备份目录本身不可读时，子命令必须显式失败而不是报"一切正常"。
func TestCLIWithUnreadableBackupDir(t *testing.T) {
	cfg := cliTestConfig(t)
	cfg.backupDir = filepath.Join(cfg.backupDir, "missing")

	captureStdout(t, func() {
		if code := cmdList(cfg, ""); code != 1 {
			t.Errorf("list 应报告目录不可读, got %d", code)
		}
	})
	captureStdout(t, func() {
		if code := cmdStatus(cfg); code != 1 {
			t.Errorf("status 应报告目录不可读, got %d", code)
		}
	})
	captureStdout(t, func() {
		if code := cmdVerify(cfg, "2026-09-21"); code != 1 {
			t.Errorf("verify 缺清单时应返回 1, got %d", code)
		}
	})
}

func TestVerifyOneFileChecksumError(t *testing.T) {
	cfg := cliTestConfig(t)
	dir := t.TempDir()
	// 对目录算校验和会失败：要报"计算校验和失败"而不是静默通过
	problems := verifyOneFile(cfg, dir, fileEntry{Size: 1, SHA256: strings.Repeat("0", 64)})
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "计算校验和失败") {
		t.Errorf("got %v", problems)
	}
}
