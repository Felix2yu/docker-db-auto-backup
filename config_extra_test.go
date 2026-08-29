package main

import (
	"testing"
)

func TestEnvOr(t *testing.T) {
	t.Setenv("FOO", "")
	if got := envOr("FOO", "fallback"); got != "fallback" {
		t.Errorf("空值应使用默认值, got %q", got)
	}
	t.Setenv("BAR", "value")
	if got := envOr("BAR", "fallback"); got != "value" {
		t.Errorf("got %q, want value", got)
	}
}

func TestEnvIsTrue(t *testing.T) {
	cases := map[string]bool{
		"true":  true,
		"TRUE":  true,
		"1":     true,
		"yes":   true,
		"YES":   true,
		"false": false,
		"0":     false,
		"":      false,
		"maybe": false,
	}
	for in, want := range cases {
		t.Setenv("FLAG", in)
		if got := envIsTrue("FLAG"); got != want {
			t.Errorf("envIsTrue(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEnvBoolDefaultTrue(t *testing.T) {
	t.Setenv("FLAG", "")
	if !envBoolDefaultTrue("FLAG") {
		t.Error("空值应默认 true")
	}
	t.Setenv("FLAG", "false")
	if envBoolDefaultTrue("FLAG") {
		t.Error("false 应为 false")
	}
	t.Setenv("FLAG", "true")
	if !envBoolDefaultTrue("FLAG") {
		t.Error("true 应为 true")
	}
}

func TestEnvInt(t *testing.T) {
	t.Setenv("N", "")
	if got := envInt("N", 7); got != 7 {
		t.Errorf("空值应使用默认, got %d", got)
	}
	t.Setenv("N", "42")
	if got := envInt("N", 7); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
	t.Setenv("N", "notanint")
	if got := envInt("N", 7); got != 7 {
		t.Errorf("无效整数应使用默认, got %d", got)
	}
}

func TestSplitTrim(t *testing.T) {
	cases := map[string][]string{
		"":             nil,
		"a":            {"a"},
		" a , b ,, c ": {"a", "b", "c"},
		" ,, ":         nil,
		"x,y, z":       {"x", "y", "z"},
	}
	for in, want := range cases {
		if got := splitTrim(in); !equalStringSlice(got, want) {
			t.Errorf("splitTrim(%q) = %v, want %v", in, got, want)
		}
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("KOPIA_REPOSITORY_TYPE", "")
	t.Setenv("BACKUP_DIR", "")
	cfg := loadConfig()
	if cfg.backupDir != "/var/backups" {
		t.Errorf("BACKUP_DIR 默认应为 /var/backups, got %q", cfg.backupDir)
	}
	if cfg.compression != "plain" {
		t.Errorf("COMPRESSION 默认应为 plain, got %q", cfg.compression)
	}
	if cfg.workers <= 0 {
		t.Errorf("workers 应大于 0, got %d", cfg.workers)
	}
	if !cfg.backupValidate {
		t.Error("BACKUP_VALIDATE 默认应为 true")
	}
}

func TestLoadConfigValues(t *testing.T) {
	t.Setenv("KOPIA_REPOSITORY_TYPE", "")
	t.Setenv("BACKUP_DIR", "/data")
	t.Setenv("SCHEDULE", "0 0 * * *")
	t.Setenv("COMPRESSION", "GZIP")
	t.Setenv("SINGLE_DB_MODE", "true")
	t.Setenv("SHOUTRRR_URLS", "ntfy://example.com/a, slack://x")
	t.Setenv("HEALTHCHECKS_URL", "https://hc.example.com/ping")
	t.Setenv("BACKUP_RETENTION_DAYS", "14")
	t.Setenv("BACKUP_WORKERS", "4")
	t.Setenv("PUID", "1000")
	t.Setenv("PGID", "1000")
	t.Setenv("BACKUP_VALIDATE", "false")
	t.Setenv("NTFY_MARKDOWN", "false")

	cfg := loadConfig()
	if cfg.backupDir != "/data" {
		t.Errorf("BACKUP_DIR = %q", cfg.backupDir)
	}
	if cfg.schedule != "0 0 * * *" {
		t.Errorf("SCHEDULE = %q", cfg.schedule)
	}
	if cfg.compression != "gzip" {
		t.Errorf("COMPRESSION 应小写化, got %q", cfg.compression)
	}
	if !cfg.singleDBMode {
		t.Error("SINGLE_DB_MODE 应为 true")
	}
	if len(cfg.shoutrrrURLs) != 2 {
		t.Errorf("SHOUTRRR_URLS 应解析为 2 个, got %v", cfg.shoutrrrURLs)
	}
	if cfg.healthchecksURL != "https://hc.example.com/ping" {
		t.Errorf("HEALTHCHECKS_URL = %q", cfg.healthchecksURL)
	}
	if cfg.retentionDays != 14 {
		t.Errorf("BACKUP_RETENTION_DAYS = %d", cfg.retentionDays)
	}
	if cfg.workers != 4 {
		t.Errorf("BACKUP_WORKERS = %d", cfg.workers)
	}
	if cfg.puid != 1000 || cfg.pgid != 1000 {
		t.Errorf("PUID/PGID = %d/%d", cfg.puid, cfg.pgid)
	}
	if cfg.backupValidate {
		t.Error("BACKUP_VALIDATE=false 应为 false")
	}
	if cfg.ntfyMarkdown {
		t.Error("NTFY_MARKDOWN=false 应为 false")
	}
}

func TestLoadKopiaConfigFull(t *testing.T) {
	t.Setenv("KOPIA_REPOSITORY_TYPE", "s3")
	t.Setenv("KOPIA_PASSWORD", "secret")
	t.Setenv("KOPIA_REPOSITORY_FLAGS", "--bucket=b --endpoint=e")
	t.Setenv("KOPIA_CREATE_REPOSITORY", "true")
	t.Setenv("KOPIA_CONFIG_FILE", "/etc/kopia/repo.config")
	t.Setenv("KOPIA_POLICY_COMPRESSION", "GZIP")
	cfg := loadKopiaConfig("/var/backups")
	if cfg == nil {
		t.Fatal("kopia 应启用")
	}
	if cfg.repositoryType != "s3" {
		t.Errorf("repositoryType = %q", cfg.repositoryType)
	}
	if cfg.password != "secret" {
		t.Errorf("password = %q", cfg.password)
	}
	if cfg.repositoryFlags != "--bucket=b --endpoint=e" {
		t.Errorf("repositoryFlags = %q", cfg.repositoryFlags)
	}
	if !cfg.createRepository {
		t.Error("createRepository 应为 true")
	}
	if cfg.configFile != "/etc/kopia/repo.config" {
		t.Errorf("configFile = %q", cfg.configFile)
	}
	if cfg.policyCompression != "gzip" {
		t.Errorf("policyCompression 应小写化, got %q", cfg.policyCompression)
	}
}

func TestLoadKopiaConfigDefaultFile(t *testing.T) {
	t.Setenv("KOPIA_REPOSITORY_TYPE", "posix")
	t.Setenv("KOPIA_CONFIG_FILE", "")
	cfg := loadKopiaConfig("/var/backups")
	if cfg == nil {
		t.Fatal("kopia 应启用")
	}
	want := "/var/backups/.kopia/repository.config"
	if cfg.configFile != want {
		t.Errorf("configFile 默认应为 %q, got %q", want, cfg.configFile)
	}
}
