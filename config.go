package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/term"
)

type config struct {
	backupDir       string
	schedule        string
	compression     string
	singleDBMode    bool
	shoutrrrURLs    []string
	healthchecksURL string
	retentionDays   int
	workers         int
	puid            int
	pgid            int
	showProgress    bool
	kopia           *kopiaConfig
}

type kopiaConfig struct {
	repositoryType    string
	password          string
	repositoryFlags   string
	createRepository  bool
	configFile        string
	policyCompression string
}

func loadConfig() *config {
	backupDir := envOr("BACKUP_DIR", "/var/backups")
	return &config{
		backupDir:       backupDir,
		schedule:        os.Getenv("SCHEDULE"),
		compression:     strings.ToLower(envOr("COMPRESSION", "plain")),
		singleDBMode:    envIsTrue("SINGLE_DB_MODE"),
		shoutrrrURLs:    splitTrim(os.Getenv("SHOUTRRR_URLS")),
		healthchecksURL: os.Getenv("HEALTHCHECKS_URL"),
		retentionDays:   envInt("BACKUP_RETENTION_DAYS", 0),
		workers:         envInt("BACKUP_WORKERS", runtime.NumCPU()),
		puid:            envInt("PUID", 0),
		pgid:            envInt("PGID", 0),
		showProgress:    term.IsTerminal(int(os.Stdout.Fd())),
		kopia:           loadKopiaConfig(backupDir),
	}
}

func loadKopiaConfig(backupDir string) *kopiaConfig {
	if os.Getenv("KOPIA_REPOSITORY_TYPE") == "" {
		return nil
	}
	configFile := os.Getenv("KOPIA_CONFIG_FILE")
	if configFile == "" {
		configFile = filepath.Join(backupDir, ".kopia", "repository.config")
	}
	return &kopiaConfig{
		repositoryType:    strings.TrimSpace(os.Getenv("KOPIA_REPOSITORY_TYPE")),
		password:          os.Getenv("KOPIA_PASSWORD"),
		repositoryFlags:   strings.TrimSpace(os.Getenv("KOPIA_REPOSITORY_FLAGS")),
		createRepository:  envIsTrue("KOPIA_CREATE_REPOSITORY"),
		configFile:        configFile,
		policyCompression: strings.ToLower(strings.TrimSpace(os.Getenv("KOPIA_POLICY_COMPRESSION"))),
	}
}

func (c *config) kopiaEnabled() bool {
	return c.kopia != nil
}

func (c *config) effectiveCompression() string {
	if c.kopiaEnabled() {
		return "plain"
	}
	return c.compression
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIsTrue(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "true", "1", "yes":
		return true
	}
	return false
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func splitTrim(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
