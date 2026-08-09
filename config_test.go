package main

import "testing"

func TestEffectiveCompressionWithoutKopia(t *testing.T) {
	cfg := &config{compression: "gzip"}
	if got := cfg.effectiveCompression(); got != "gzip" {
		t.Errorf("got %q, want gzip", got)
	}
}

func TestEffectiveCompressionWithKopia(t *testing.T) {
	cfg := &config{compression: "gzip", kopia: &kopiaConfig{repositoryType: "s3"}}
	if !cfg.kopiaEnabled() {
		t.Fatal("kopia should be enabled")
	}
	if got := cfg.effectiveCompression(); got != "plain" {
		t.Errorf("got %q, want plain", got)
	}
}

func TestLoadKopiaConfigDisabled(t *testing.T) {
	t.Setenv("KOPIA_REPOSITORY_TYPE", "")
	if got := loadKopiaConfig("/var/backups"); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestLoadKopiaConfig(t *testing.T) {
	t.Setenv("KOPIA_REPOSITORY_TYPE", "s3")
	t.Setenv("KOPIA_PASSWORD", "secret")
	t.Setenv("KOPIA_REPOSITORY_FLAGS", "--bucket=abc --endpoint=http://localhost:9000")
	t.Setenv("KOPIA_CREATE_REPOSITORY", "true")
	t.Setenv("KOPIA_CONFIG_FILE", "")

	cfg := loadKopiaConfig("/var/backups")
	if cfg == nil {
		t.Fatal("expected kopia config")
	}
	if cfg.repositoryType != "s3" {
		t.Errorf("repositoryType: got %q", cfg.repositoryType)
	}
	if cfg.password != "secret" {
		t.Errorf("password: got %q", cfg.password)
	}
	if cfg.createRepository != true {
		t.Error("createRepository should be true")
	}
	wantConfig := "/var/backups/.kopia/repository.config"
	if cfg.configFile != wantConfig {
		t.Errorf("configFile: got %q, want %q", cfg.configFile, wantConfig)
	}
}

func TestLoadKopiaConfigCustomConfigFile(t *testing.T) {
	t.Setenv("KOPIA_REPOSITORY_TYPE", "posix")
	t.Setenv("KOPIA_CONFIG_FILE", "/etc/kopia/repository.config")
	cfg := loadKopiaConfig("/var/backups")
	if cfg == nil || cfg.configFile != "/etc/kopia/repository.config" {
		t.Errorf("unexpected config file: %+v", cfg)
	}
}
