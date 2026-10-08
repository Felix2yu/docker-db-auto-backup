package main

import (
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// freeAddr 占一个端口再立刻释放，拿到一个大概率空闲的地址给指标服务用。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitForGET(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			body := make([]byte, 64<<10)
			n, _ := resp.Body.Read(body)
			resp.Body.Close()
			return resp, string(body[:n])
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 就绪超时: %v", url, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInitLoggerFormatsAndLevels(t *testing.T) {
	defer initLogger(&config{logLevel: "info", logFormat: "text"})

	for _, c := range []*config{
		{logLevel: "debug", logFormat: "json"},
		{logLevel: "warn", logFormat: "text"},
		{logLevel: "warning", logFormat: "text"},
		{logLevel: "error", logFormat: "JSON"},
		{logLevel: "", logFormat: ""},
	} {
		initLogger(c)
		// 各级别都调用一遍，让被过滤掉的分支也走一次
		logDebug("debug 消息")
		logInfo("info 消息")
		logWarn("warn 消息")
		logError("error 消息")
	}
}

func TestLogProgressWritesDirectly(t *testing.T) {
	out := captureStdout(t, func() { logProgress("进度 %d\n", 42) })
	if strings.TrimSpace(out) != "进度 42" {
		t.Errorf("got %q", out)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	addr := freeAddr(t)
	startMetricsServer(addr)

	initMetrics()
	addCounter("db_backup_runs_total", 3)
	setGauge("db_backup_last_status", 0.5)

	_, body := waitForGET(t, "http://"+addr+"/metrics")
	if !strings.Contains(body, "db_backup_runs_total 3") {
		t.Errorf("计数器未导出:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE db_backup_last_status gauge") {
		t.Errorf("指标类型未导出:\n%s", body)
	}
	if !strings.Contains(body, "db_backup_bytes_total") {
		t.Errorf("缺少体积指标:\n%s", body)
	}

	resp, body := waitForGET(t, "http://"+addr+"/healthz")
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Errorf("healthz = %d %q", resp.StatusCode, body)
	}
}

func TestMetricsServerPortBusyIsLoggedNotFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// 端口被占用时后台 goroutine 只会记日志，不能 panic 或阻塞调用方
	startMetricsServer(ln.Addr().String())
	time.Sleep(100 * time.Millisecond)
}

func TestUnknownMetricNamesAreIgnored(t *testing.T) {
	before := renderMetrics()
	setGauge("不存在的指标", 1)
	addCounter("也不存在的计数器", 1)
	addCounter("db_backup_runs_total", 0)
	addCounter("db_backup_runs_total", -1)
	if after := renderMetrics(); after != before {
		t.Errorf("未注册的指标不应改变输出:\n%s\n%s", before, after)
	}
}

func TestRenderMetricsConcurrentSafety(t *testing.T) {
	initMetrics()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addCounter("db_backup_container_failures_total", 1)
			setGauge("db_backup_last_duration_seconds", float64(i))
			_ = renderMetrics()
			logInfo("并发日志", "i", i)
			logProgress("")
		}(i)
	}
	wg.Wait()
	if !strings.Contains(renderMetrics(), "db_backup_container_failures_total 8") {
		t.Errorf("并发累加应到 8:\n%s", renderMetrics())
	}
}

func TestHumanBytesAllUnits(t *testing.T) {
	cases := map[int64]string{
		0:             "0 B",
		512:           "512 B",
		1024:          "1.0 KiB",
		1536:          "1.5 KiB",
		1 << 20:       "1.0 MiB",
		1 << 30:       "1.0 GiB",
		1 << 40:       "1.0 TiB",
		1 << 50:       "1.0 PiB",
		6 * (1 << 50): "6.0 PiB",
		1 << 60:       "1.0 EiB",
		math.MaxInt64: "8.0 EiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatDurationBoundaries(t *testing.T) {
	if got := formatDuration(time.Second); got != "1.00 秒" {
		t.Errorf("got %q", got)
	}
	if got := formatDuration(0); got != "0.00 秒" {
		t.Errorf("got %q", got)
	}
}

func TestStatusPartialCountAndLabel(t *testing.T) {
	if statusPartialCount(statusPartial) != 1 || statusPartialCount(statusSuccess) != 0 {
		t.Error("partial 计数不对")
	}
	if got := statusLabel("  spaced  "); got != "spaced" {
		t.Errorf("未知状态应原样去掉空白, got %q", got)
	}
	if got := statusLabel(""); got != "" {
		t.Errorf("got %q", got)
	}
}

// hcRecorder 收集心跳请求，便于断言 start/fail/ok 三种状态。
type hcRecorder struct {
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func newHCRecorder(t *testing.T) *hcRecorder {
	t.Helper()
	r := &hcRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.bodies = append(r.bodies, string(body))
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *hcRecorder) requests() ([]string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...), strings.Join(r.bodies, "\n")
}

// 心跳 URL 的三种状态都要真的发出去，且失败原因要带在正文里。
func TestHealthchecksPingsIncludeReason(t *testing.T) {
	rec := newHCRecorder(t)

	cfg := &config{healthchecksURL: rec.srv.URL + "/ping/abc/"}
	hc := newHealthchecks(cfg)
	hc.start()
	hc.fail("磁盘空间不足")
	hc.ok("备份成功")

	paths, bodies := rec.requests()
	joined := strings.Join(paths, ",")
	for _, want := range []string{"/ping/abc/start", "/ping/abc/fail", "/ping/abc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("应请求 %s, got %s", want, joined)
		}
	}
	if !strings.Contains(bodies, "磁盘空间不足") {
		t.Errorf("失败原因应作为正文上报: %q", bodies)
	}
	if !strings.Contains(bodies, "备份成功") {
		t.Errorf("成功正文应上报: %q", bodies)
	}

	// 未配置 URL 时不应发起任何请求
	newHealthchecks(&config{}).fail("不该发出")
	if again, _ := rec.requests(); len(again) != len(paths) {
		t.Error("未启用心跳时不应请求网络")
	}
}

// 心跳服务不可达时只记日志，不能把备份本身带崩。
func TestHealthchecksUnreachable(t *testing.T) {
	rec := newHCRecorder(t)
	url := rec.srv.URL
	rec.srv.Close() // 立刻关掉，制造连接失败

	hc := newHealthchecks(&config{healthchecksURL: url})
	hc.start()
	hc.ok("done")
	hc.fail("done")
}
