package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"1G":    1 << 30,
		"512M":  512 << 20,
		"2T":    2 << 40,
		"1024":  1024,
		"100B":  100,
		"1GiB":  1 << 30,
		" 64M ": 64 << 20,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseSize("abc"); err == nil {
		t.Error("非法容量应返回错误")
	}
}

func TestHumanBytes(t *testing.T) {
	if got := humanBytes(512); got != "512 B" {
		t.Errorf("humanBytes(512) = %q", got)
	}
	if got := humanBytes(1<<20 + 512<<10); !strings.Contains(got, "MiB") {
		t.Errorf("humanBytes 应使用 MiB, got %q", got)
	}
}

func TestDetectAnomalies(t *testing.T) {
	prev := &backupManifest{
		Date: "2026-09-20",
		Containers: []containerManifest{
			{Name: "pg", Provider: "postgres", Files: []fileEntry{{Path: "a.sql", Size: 1000}}},
			{Name: "my", Provider: "mysql", Files: []fileEntry{{Path: "b.sql", Size: 1000}}},
		},
	}
	cur := &backupManifest{
		Date: "2026-09-21",
		Containers: []containerManifest{
			{Name: "pg", Provider: "postgres", Files: []fileEntry{{Path: "a.sql", Size: 100}}},
		},
	}
	warnings := detectAnomalies(prev, cur, 0.5)
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "my") {
		t.Errorf("容器缺失应被检测: %v", warnings)
	}
	if !strings.Contains(joined, "pg") || !strings.Contains(joined, "下降") {
		t.Errorf("体积骤降应被检测: %v", warnings)
	}
	if len(detectAnomalies(nil, cur, 0.5)) != 0 {
		t.Error("无历史清单时不应产生告警")
	}
}

func TestFormatReportIncludesMetrics(t *testing.T) {
	m := &backupManifest{
		Date:   "2026-09-21",
		Status: statusPartial,
		Kopia:  &kopiaManifestInfo{Pushed: true, SnapshotID: "snap-1"},
		Containers: []containerManifest{
			{Name: "pg", Provider: "postgres", Mode: modeFullFallback,
				Files: []fileEntry{{Path: "2026-09-21/pg.sql", Size: 2048}}},
		},
		Failures: []string{"rd: 导出失败"},
		Warnings: []string{"体积下降"},
	}
	body := formatReport(m, "3.20 秒")
	for _, want := range []string{"部分失败", "2026-09-21", "3.20 秒", "snap-1", "单库模式回退", "2.0 KiB", "rd: 导出失败", "体积下降"} {
		if !strings.Contains(body, want) {
			t.Errorf("通知正文缺少 %q:\n%s", want, body)
		}
	}
}

func TestStatusGaugeAndMetrics(t *testing.T) {
	initMetrics()
	setGauge("db_backup_last_status", statusGauge(statusPartial))
	addCounter("db_backup_runs_total", 2)
	out := renderMetrics()
	for _, want := range []string{"db_backup_runs_total", "db_backup_last_status", "gauge", "counter"} {
		if !strings.Contains(out, want) {
			t.Errorf("指标输出缺少 %q:\n%s", want, out)
		}
	}
	if statusGauge(statusSuccess) != 1 || statusGauge(statusFailed) != 0 || statusGauge(statusPartial) != 0.5 {
		t.Error("状态映射不符合预期")
	}
}

func TestFileExtFromName(t *testing.T) {
	cases := map[string]string{
		"pg.sql":     "sql",
		"pg.sql.gz":  "sql",
		"pg.sql.xz":  "sql",
		"rd.rdb":     "rdb",
		"rd.rdb.bz2": "rdb",
		"/a/b/c.sql": "sql",
	}
	for in, want := range cases {
		if got := fileExtFromName(in); got != want {
			t.Errorf("fileExtFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifyOneFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.sql")
	content := "-- PostgreSQL database dump\nCREATE TABLE t (id int);\n-- PostgreSQL database dump complete\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config{compression: "plain"}

	good := fileEntry{Path: path, Size: int64(len(content))}
	if problems := verifyOneFile(cfg, path, good); len(problems) != 0 {
		t.Errorf("完好文件不应报错: %v", problems)
	}

	// 体积不符
	bad := fileEntry{Path: path, Size: 1}
	if problems := verifyOneFile(cfg, path, bad); len(problems) == 0 {
		t.Error("体积不符应被检出")
	}

	// 校验和不符
	sum, _ := fileChecksum(path)
	badSum := fileEntry{Path: path, Size: int64(len(content)), SHA256: "deadbeef"}
	if problems := verifyOneFile(cfg, path, badSum); len(problems) == 0 {
		t.Error("校验和不符应被检出")
	}
	if !strings.Contains(strings.Join(verifyOneFile(cfg, path, badSum), ""), "SHA256") {
		t.Error("应明确提示 SHA256 不一致")
	}

	// 校验和一致
	okSum := fileEntry{Path: path, Size: int64(len(content)), SHA256: sum}
	if problems := verifyOneFile(cfg, path, okSum); len(problems) != 0 {
		t.Errorf("校验和一致时不应报错: %v", problems)
	}
}

func TestLoadExtraProviders(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "patterns.json")
	content := `{"patterns":[
		{"pattern":"myorg/postgres-fork","provider":"postgres"},
		{"pattern":"myorg/mydb","provider":"mysql","command":["mysqldump","--all-databases"]}
	]}`
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// 全局 providers 会被修改，测试结束后恢复
	original := providers
	defer func() { providers = original }()

	if err := loadExtraProviders(file); err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	p := getBackupProvider([]string{"myorg/postgres-fork"})
	if p == nil || p.name != "postgres" {
		t.Fatalf("扩展规则未生效: %+v", p)
	}
	custom := getBackupProvider([]string{"myorg/mydb"})
	if custom == nil || custom.name != "mysql" {
		t.Fatalf("自定义 provider 未注册: %+v", custom)
	}
	cmd, _, err := custom.backupMethod(nil, nil, nil, "")
	if err != nil || strings.Join(cmd, " ") != "mysqldump --all-databases" {
		t.Errorf("自定义命令异常: %v %v", cmd, err)
	}

	// 空路径不报错
	if err := loadExtraProviders(""); err != nil {
		t.Errorf("空路径应直接返回: %v", err)
	}
	// 不存在的文件应报错
	if err := loadExtraProviders(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("文件不存在时应返回错误")
	}
}

func TestImageNamesFromDigests(t *testing.T) {
	got := imageNamesFromDigests([]string{"postgres@sha256:abc", "postgres@sha256:def", "redis@sha256:1"})
	if len(got) != 2 || got[0] != "postgres" || got[1] != "redis" {
		t.Errorf("去重/解析异常: %v", got)
	}
	if len(imageNamesFromDigests(nil)) != 0 {
		t.Error("空输入应返回空")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &backupManifest{
		Date:   "2026-09-21",
		RunAt:  time.Now(),
		Status: statusSuccess,
		Containers: []containerManifest{{
			Name: "pg", Provider: "postgres", Mode: modeSingle,
			Files: []fileEntry{{Path: "2026-09-21/pg/appdb.sql", Database: "appdb", Size: 42}},
		}},
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	got, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if got.Status != statusSuccess || got.fileCount() != 1 || got.totalBytes() != 42 {
		t.Errorf("回读异常: %+v", got)
	}
	if _, err := readManifest(filepath.Join(dir, "nope")); err == nil {
		t.Error("不存在的目录应返回错误")
	}
}

func TestFindPreviousManifest(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"2026-09-19", "2026-09-20", "2026-09-21"} {
		sub := filepath.Join(dir, d)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if d == "2026-09-19" {
			continue // 无清单
		}
		if err := writeManifest(sub, &backupManifest{Date: d, Status: statusSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	prev, err := findPreviousManifest(dir, "2026-09-21")
	if err != nil || prev == nil || prev.Date != "2026-09-20" {
		t.Errorf("应跳过无清单的日期并取最近一次: %+v %v", prev, err)
	}
}
