package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// 结构化日志（B8）。默认输出到 stdout，可通过 LOG_FORMAT=json 切换为 JSON，
// 便于接入日志采集系统（Docker 自身会补时间戳）。
var (
	logMu  sync.RWMutex
	logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
)

func initLogger(cfg *config) {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var h slog.Handler
	if strings.EqualFold(cfg.logFormat, "json") {
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}

	logMu.Lock()
	logger = slog.New(h)
	logMu.Unlock()
	slog.SetDefault(logger)
}

func logDebug(msg string, args ...any) {
	logMu.RLock()
	l := logger
	logMu.RUnlock()
	l.Debug(msg, args...)
}

func logInfo(msg string, args ...any) {
	logMu.RLock()
	l := logger
	logMu.RUnlock()
	l.Info(msg, args...)
}

func logWarn(msg string, args ...any) {
	logMu.RLock()
	l := logger
	logMu.RUnlock()
	l.Warn(msg, args...)
}

func logError(msg string, args ...any) {
	logMu.RLock()
	l := logger
	logMu.RUnlock()
	l.Error(msg, args...)
}

// 用户可见的进度信息：无论日志级别如何都直接打印，保持原有交互体验。
func logProgress(format string, args ...any) {
	fmt.Printf(format, args...)
}

// ---------------------------------------------------------------------------
// Prometheus 指标（B8）
//
// 为避免引入第三方依赖，这里实现了一个最小的指标注册表与 /metrics 文本导出，
// 输出格式与 Prometheus text exposition format 兼容。
// ---------------------------------------------------------------------------

type metricKind string

const (
	metricGauge   metricKind = "gauge"
	metricCounter metricKind = "counter"
)

type metricDef struct {
	name  string
	help  string
	kind  metricKind
	value float64
}

var (
	metricMu sync.Mutex
	metrics  = map[string]*metricDef{}
)

func registerMetric(name, help string, kind metricKind) {
	metricMu.Lock()
	metrics[name] = &metricDef{name: name, help: help, kind: kind}
	metricMu.Unlock()
}

func setGauge(name string, v float64) {
	metricMu.Lock()
	if m, ok := metrics[name]; ok {
		m.value = v
	}
	metricMu.Unlock()
}

func addCounter(name string, delta float64) {
	metricMu.Lock()
	if m, ok := metrics[name]; ok && delta > 0 {
		m.value += delta
	}
	metricMu.Unlock()
}

func initMetrics() {
	registerMetric("db_backup_runs_total", "备份执行总次数", metricCounter)
	registerMetric("db_backup_failures_total", "备份失败次数（全部失败）", metricCounter)
	registerMetric("db_backup_partial_failures_total", "备份部分失败次数", metricCounter)
	registerMetric("db_backup_container_failures_total", "单个容器备份失败累计次数", metricCounter)
	registerMetric("db_backup_last_run_timestamp_seconds", "上次备份开始时间", metricGauge)
	registerMetric("db_backup_last_success_timestamp_seconds", "上次完全成功时间", metricGauge)
	registerMetric("db_backup_last_duration_seconds", "上次备份耗时（秒）", metricGauge)
	registerMetric("db_backup_last_status", "上次备份状态：1 成功 / 0.5 部分失败 / 0 失败", metricGauge)
	registerMetric("db_backup_containers_total", "上次备份的容器数", metricGauge)
	registerMetric("db_backup_containers_failed_total", "上次备份失败的容器数", metricGauge)
	registerMetric("db_backup_files_total", "上次备份的文件数", metricGauge)
	registerMetric("db_backup_bytes_total", "上次备份的总字节数", metricGauge)
}

func renderMetrics() string {
	metricMu.Lock()
	names := make([]string, 0, len(metrics))
	for name := range metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		m := metrics[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %g\n",
			m.name, m.help, m.name, m.kind, m.name, m.value)
	}
	metricMu.Unlock()
	return b.String()
}

// startMetricsServer 在后台启动 /metrics 端点，addr 为空则不启动。
func startMetricsServer(addr string) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, renderMetrics())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		logInfo("指标端点已启动", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError("指标端点异常退出", "addr", addr, "error", err)
		}
	}()
}
