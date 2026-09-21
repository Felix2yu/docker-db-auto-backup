package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

type config struct {
	backupDir       string
	schedule        string
	compression     string
	singleDBMode    bool
	notifyURLs      []string
	healthchecksURL string
	retentionDays   int
	workers         int
	puid            int
	pgid            int
	showProgress    bool
	backupValidate  bool
	ntfyMarkdown    bool
	kopia           *kopiaConfig

	// A2：单任务超时与重试
	backupTimeout time.Duration
	backupRetries int
	retryBackoff  time.Duration

	// B5：备份范围控制
	includeContainers []string
	excludeContainers []string
	includeLabels     []string

	// B7：恢复演练
	restoreDrill bool

	// B8：日志与指标
	logFormat   string
	logLevel    string
	metricsAddr string

	// B10：镜像识别扩展
	patternsFile string

	// B4：磁盘预检
	minFreeBytes int64

	// B2/B9：清单与异常检测
	checksumEnabled bool
	sizeDriftRatio  float64

	// C5：显式凭据
	mysqlUser     string
	mysqlPassword string
	pgUser        string

	// C8：时区
	loc *time.Location
}

type kopiaConfig struct {
	repositoryType      string
	password            string
	repositoryFlags     string
	createRepository    bool
	configFile          string
	policyCompression   string
	retentionFlags      string
	maintenanceInterval time.Duration
}

func loadConfig() *config {
	backupDir := envOr("BACKUP_DIR", "/var/backups")

	workers := envInt("BACKUP_WORKERS", 0)
	if workers <= 0 {
		// A3：备份瓶颈在数据库容器与磁盘 IO，不在备份容器 CPU。
		// 默认并发度取 min(NumCPU, 2)，避免同时冲击多个线上库。
		workers = runtime.NumCPU()
		if workers > 2 {
			workers = 2
		}
	}

	cfg := &config{
		backupDir:       backupDir,
		schedule:        os.Getenv("SCHEDULE"),
		compression:     strings.ToLower(envOr("COMPRESSION", "plain")),
		singleDBMode:    envIsTrue("SINGLE_DB_MODE"),
		notifyURLs:      splitTrim(os.Getenv("NOTIFY_URLS")),
		healthchecksURL: os.Getenv("HEALTHCHECKS_URL"),
		retentionDays:   envInt("BACKUP_RETENTION_DAYS", 0),
		workers:         workers,
		puid:            envInt("PUID", 0),
		pgid:            envInt("PGID", 0),
		showProgress:    envBoolDefaultAuto("SHOW_PROGRESS", term.IsTerminal(int(os.Stdout.Fd()))),
		backupValidate:  envBoolDefaultTrue("BACKUP_VALIDATE"),
		ntfyMarkdown:    envBoolDefaultTrue("NTFY_MARKDOWN"),
		kopia:           loadKopiaConfig(backupDir),

		backupTimeout: envDuration("BACKUP_TIMEOUT_MINUTES", 60*time.Minute),
		backupRetries: envInt("BACKUP_RETRIES", 2),
		retryBackoff:  envDuration("BACKUP_RETRY_BACKOFF_SECONDS", 15*time.Second),

		includeContainers: splitTrim(os.Getenv("BACKUP_INCLUDE_CONTAINERS")),
		excludeContainers: splitTrim(os.Getenv("BACKUP_EXCLUDE_CONTAINERS")),
		includeLabels:     splitTrim(os.Getenv("BACKUP_INCLUDE_LABELS")),

		restoreDrill: envIsTrue("BACKUP_RESTORE_DRILL"),

		logFormat:   strings.ToLower(envOr("LOG_FORMAT", "text")),
		logLevel:    strings.ToLower(envOr("LOG_LEVEL", "info")),
		metricsAddr: os.Getenv("METRICS_ADDR"),

		patternsFile: os.Getenv("BACKUP_IMAGE_PATTERNS_FILE"),

		minFreeBytes: envSize("BACKUP_MIN_FREE_SPACE", 1<<30),

		checksumEnabled: envBoolDefaultTrue("BACKUP_CHECKSUM"),
		sizeDriftRatio:  envFloat("BACKUP_SIZE_DRIFT_RATIO", 0.5),

		mysqlUser:     os.Getenv("MYSQL_BACKUP_USER"),
		mysqlPassword: os.Getenv("MYSQL_BACKUP_PASSWORD"),
		pgUser:        os.Getenv("POSTGRES_BACKUP_USER"),
	}

	cfg.loc = time.Local
	if tz := strings.TrimSpace(os.Getenv("BACKUP_TZ")); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			fmt.Printf("警告: 无法解析 BACKUP_TZ=%q（%v），将使用系统本地时区\n", tz, err)
		} else {
			cfg.loc = loc
		}
	}
	if cfg.backupRetries < 0 {
		cfg.backupRetries = 0
	}
	return cfg
}

func loadKopiaConfig(backupDir string) *kopiaConfig {
	if os.Getenv("KOPIA_REPOSITORY_TYPE") == "" {
		return nil
	}
	configFile := os.Getenv("KOPIA_CONFIG_FILE")
	if configFile == "" {
		configFile = filepath.Join(backupDir, ".kopia", "repository.config")
	}
	kc := &kopiaConfig{
		repositoryType:    strings.TrimSpace(os.Getenv("KOPIA_REPOSITORY_TYPE")),
		password:          os.Getenv("KOPIA_PASSWORD"),
		repositoryFlags:   strings.TrimSpace(os.Getenv("KOPIA_REPOSITORY_FLAGS")),
		createRepository:  envIsTrue("KOPIA_CREATE_REPOSITORY"),
		configFile:        configFile,
		policyCompression: strings.ToLower(strings.TrimSpace(os.Getenv("KOPIA_POLICY_COMPRESSION"))),
		retentionFlags:    strings.TrimSpace(os.Getenv("KOPIA_RETENTION_FLAGS")),
	}

	// B6：维护任务默认每 24 小时执行一次；设为 0 表示关闭。
	hours := envFloat("KOPIA_MAINTENANCE_INTERVAL_HOURS", 24)
	if hours <= 0 {
		kc.maintenanceInterval = 0
	} else {
		kc.maintenanceInterval = time.Duration(hours * float64(time.Hour))
	}
	return kc
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

// location 返回用于日期目录与调度的时区（C8）。
func (c *config) location() *time.Location {
	if c.loc == nil {
		return time.Local
	}
	return c.loc
}

// nowIn 返回配置时区下的当前时间。
func (c *config) nowIn() time.Time {
	return time.Now().In(c.location())
}

// tmpDir 返回备份临时文件目录（C7）：与备份产物隔离，避免半成品被 Kopia 快照。
func (c *config) tmpDir() string {
	return filepath.Join(c.backupDir, ".tmp")
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

func envBoolDefaultTrue(key string) bool {
	if v := os.Getenv(key); v == "" {
		return true
	}
	return envIsTrue(key)
}

// envBoolDefaultAuto 在变量未设置时回退到自动判定结果（用于 SHOW_PROGRESS）。
func envBoolDefaultAuto(key string, auto bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return auto
	}
	return envIsTrue(key)
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

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

// envDuration 支持 "30m" / "1h" 这类 Go duration 写法，也支持纯数字（默认按分钟）。
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Minute
	}
	return fallback
}

// envSize 解析容量字符串，支持纯字节数与带单位的写法（1G / 512M / 2T）。
func envSize(key string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if n, err := parseSize(v); err == nil {
		return n
	}
	return fallback
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, fmt.Errorf("空的容量值")
	}
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "KIB"):
		multiplier, s = 1<<10, s[:len(s)-3]
	case strings.HasSuffix(s, "MIB"):
		multiplier, s = 1<<20, s[:len(s)-3]
	case strings.HasSuffix(s, "GIB"):
		multiplier, s = 1<<30, s[:len(s)-3]
	case strings.HasSuffix(s, "TIB"):
		multiplier, s = 1<<40, s[:len(s)-3]
	case strings.HasSuffix(s, "K"):
		multiplier, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		multiplier, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "G"):
		multiplier, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "T"):
		multiplier, s = 1<<40, s[:len(s)-1]
	case strings.HasSuffix(s, "B"):
		s = s[:len(s)-1]
	}
	s = strings.TrimSpace(s)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析容量 %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("容量不能为负: %q", s)
	}
	return n * multiplier, nil
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
