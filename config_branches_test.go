package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestEnvDurationAcceptedForms(t *testing.T) {
	t.Setenv("T", "")
	if got := envDuration("T", 5*time.Minute); got != 5*time.Minute {
		t.Errorf("未设置时应取默认, got %s", got)
	}
	for in, want := range map[string]time.Duration{
		"45":    45 * time.Minute, // 纯数字按分钟
		"30s":   30 * time.Second,
		"2h":    2 * time.Hour,
		"1h30m": 90 * time.Minute,
	} {
		t.Setenv("T", in)
		if got := envDuration("T", time.Hour); got != want {
			t.Errorf("envDuration(%q) = %s, want %s", in, got, want)
		}
	}
	t.Setenv("T", "半小时")
	if got := envDuration("T", 7*time.Minute); got != 7*time.Minute {
		t.Errorf("无法解析时应回退默认, got %s", got)
	}
}

func TestEnvSizeAndFloatFallbacks(t *testing.T) {
	t.Setenv("S", "")
	if got := envSize("S", 123); got != 123 {
		t.Errorf("got %d", got)
	}
	t.Setenv("S", "1G")
	if got := envSize("S", 123); got != 1<<30 {
		t.Errorf("got %d", got)
	}
	t.Setenv("S", "1XB")
	if got := envSize("S", 456); got != 456 {
		t.Errorf("解析失败应回退默认, got %d", got)
	}

	t.Setenv("F", "")
	if got := envFloat("F", 1.5); got != 1.5 {
		t.Errorf("got %v", got)
	}
	t.Setenv("F", "0.25")
	if got := envFloat("F", 1.5); got != 0.25 {
		t.Errorf("got %v", got)
	}
	t.Setenv("F", "一半")
	if got := envFloat("F", 2.5); got != 2.5 {
		t.Errorf("got %v", got)
	}

	t.Setenv("B", "false")
	if envBoolDefaultAuto("B", true) {
		t.Error("显式 false 应关闭自动判定")
	}
	t.Setenv("B", "")
	if !envBoolDefaultAuto("B", true) {
		t.Error("未设置时应沿用自动判定")
	}
}

func TestParseSizeUnits(t *testing.T) {
	cases := map[string]int64{
		"1024":   1024,
		"1K":     1 << 10,
		"2KIB":   2 << 10,
		"3M":     3 << 20,
		"4MIB":   4 << 20,
		"5G":     5 << 30,
		"6GiB":   6 << 30,
		"7T":     7 << 40,
		"8TIB":   8 << 40,
		"9B":     9,
		" 10 M ": 10 << 20,
		"0":      0,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}

	for _, bad := range []string{"", "   ", "-1", "abc", "M", "1.5G"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) 应报错", bad)
		}
	}
}

func TestLocationFallbackAndNowIn(t *testing.T) {
	cfg := &config{}
	if cfg.location() != time.Local {
		t.Error("未设置时区时应回退到本地时区")
	}
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatal(err)
	}
	cfg.loc = loc
	if cfg.location().String() != "UTC" {
		t.Errorf("got %s", cfg.location())
	}
	if _, offset := cfg.nowIn().Zone(); offset != 0 {
		t.Errorf("nowIn 应返回配置时区下的时间, got %v", cfg.nowIn())
	}
}

func TestLoadConfigTimezoneAndClamps(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BACKUP_DIR", dir)
	t.Setenv("KOPIA_REPOSITORY_TYPE", "")
	t.Setenv("SHOW_PROGRESS", "false")

	t.Setenv("BACKUP_TZ", "Asia/Shanghai")
	t.Setenv("BACKUP_RETRIES", "-3")
	t.Setenv("BACKUP_WORKERS", "0")
	cfg := loadConfig()
	if cfg.location().String() != "Asia/Shanghai" {
		t.Errorf("时区未按配置生效: %s", cfg.location())
	}
	if cfg.backupRetries != 0 {
		t.Errorf("负数重试应归零, got %d", cfg.backupRetries)
	}
	if cfg.workers < 1 {
		t.Errorf("workers 至少为 1, got %d", cfg.workers)
	}
	if cfg.showProgress {
		t.Error("SHOW_PROGRESS=false 应关闭进度条")
	}
	if cfg.kopiaEnabled() {
		t.Error("未设置仓库类型时不应启用 Kopia")
	}
	if cfg.effectiveCompression() != "plain" {
		t.Errorf("got %s", cfg.effectiveCompression())
	}

	// 非法时区只警告并回退，不阻塞启动
	t.Setenv("BACKUP_TZ", "Mars/Olympus")
	cfg = loadConfig()
	if cfg.location() != time.Local {
		t.Errorf("非法时区应回退本地, got %s", cfg.location())
	}

	t.Setenv("BACKUP_TZ", "")
	t.Setenv("COMPRESSION", "GZIP")
	t.Setenv("BACKUP_WORKERS", "5")
	t.Setenv("BACKUP_RETRIES", "1")
	t.Setenv("BACKUP_RETENTION_DAYS", "7")
	t.Setenv("PUID", "1000")
	t.Setenv("PGID", "1000")
	t.Setenv("SCHEDULE", "0 3 * * *")
	t.Setenv("KOPIA_REPOSITORY_TYPE", "posix")
	t.Setenv("KOPIA_PASSWORD", "pw")
	t.Setenv("KOPIA_MAINTENANCE_INTERVAL_HOURS", "0")
	cfg = loadConfig()
	if cfg.compression != "gzip" || cfg.retentionDays != 7 || cfg.puid != 1000 {
		t.Errorf("配置未按预期解析: %+v", cfg)
	}
	if cfg.workers != 5 {
		t.Errorf("got %d", cfg.workers)
	}
	if !cfg.kopiaEnabled() || cfg.kopia.maintenanceInterval != 0 {
		t.Errorf("维护间隔 0 应表示关闭: %+v", cfg.kopia)
	}
	if cfg.effectiveCompression() != "plain" {
		t.Error("启用 Kopia 时应改为交由仓库压缩")
	}
	if cfg.tmpDir() != filepath.Join(dir, ".tmp") {
		t.Errorf("tmpDir = %s", cfg.tmpDir())
	}
}
