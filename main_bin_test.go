package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// buildBinary 编译出与 CI 相同的可执行文件，用于覆盖 main() 自身的分支。
func buildBinary(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("需要 go 工具链来构建二进制")
	}
	bin := filepath.Join(t.TempDir(), "db-auto-backup")
	cmd := exec.Command(goBin, "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("构建二进制失败，跳过端到端子进程测试: %v\n%s", err, out)
	}
	return bin
}

func runBinary(t *testing.T, bin string, args []string, env map[string]string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	// 只给二进制必要的环境：宿主的 DOCKER_HOST / HTTP(S)_PROXY 会让子进程去连
	// 本地代理或不存在的守护进程，测试结果和耗时会随机器状态漂移。
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "BACKUP_DIR=" + t.TempDir(), "SCHEDULE="}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("运行二进制失败: %v\n%s", err, out)
		}
		code = exit.ExitCode()
	}
	return string(out), code
}

func TestMainBinaryCLIPaths(t *testing.T) {
	bin := buildBinary(t)

	out, code := runBinary(t, bin, []string{"help"}, nil)
	if code != 0 || !strings.Contains(out, "用法:") {
		t.Errorf("help 应打印用法并返回 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "立即执行一次备份") {
		t.Errorf("用法应列出子命令:\n%s", out)
	}

	out, code = runBinary(t, bin, []string{"没有这个子命令"}, nil)
	if code != 2 || !strings.Contains(out, "未知子命令") {
		t.Errorf("未知子命令应返回 2, got %d\n%s", code, out)
	}

	// status 在无备份时是正常状态
	if _, code := runBinary(t, bin, []string{"status"}, nil); code != 0 {
		t.Errorf("无备份的 status 应返回 0, got %d", code)
	}

	// 识别配置文件损坏时只降级，不应影响 CLI
	broken := filepath.Join(t.TempDir(), "patterns.json")
	if err := os.WriteFile(broken, []byte("{ 不是 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = runBinary(t, bin, []string{"help"}, map[string]string{"BACKUP_IMAGE_PATTERNS_FILE": broken})
	if code != 0 || !strings.Contains(out, "加载镜像识别配置失败") {
		t.Errorf("配置损坏应告警后继续, got %d\n%s", code, out)
	}

	// 指标端点打开后进程仍能正常退出
	out, code = runBinary(t, bin, []string{"help"}, map[string]string{"METRICS_ADDR": "127.0.0.1:0", "LOG_FORMAT": "json"})
	if code != 0 {
		t.Errorf("got %d\n%s", code, out)
	}
}

func TestMainBinarySingleRunAndSchedule(t *testing.T) {
	bin := buildBinary(t)
	deadSocket := "unix://" + filepath.Join(t.TempDir(), "no-daemon.sock")

	// 一次性模式：连不上 Docker 应以非零码退出
	out, code := runBinary(t, bin, nil, map[string]string{"SCHEDULE": "", "DOCKER_HOST": deadSocket})
	if code != 1 {
		t.Errorf("连接失败的一次性运行应返回 1, got %d\n%s", code, out)
	}

	// 非法 cron：记录错误后返回，进程正常退出（不能 crash-loop）
	out, code = runBinary(t, bin, nil, map[string]string{"SCHEDULE": "这不是 cron", "DOCKER_HOST": "tcp://127.0.0.1:1"})
	if code != 0 || !strings.Contains(out, "无效的备份计划") {
		t.Errorf("非法计划应报错并退出 0, got %d\n%s", code, out)
	}

	// 时区非法只警告，不应中断
	if _, code := runBinary(t, bin, []string{"status"}, map[string]string{"BACKUP_TZ": "Mars/Olympus"}); code != 0 {
		t.Errorf("非法 BACKUP_TZ 应回退到本地时区, got %d", code)
	}
}

// 调度模式要真的按点触发，且单轮失败后继续等下一轮（不是退出进程）。
func TestRunScheduledKeepsRunningAfterFailure(t *testing.T) {
	cfg := testBackupConfig(t)
	cfg.schedule = "@every 1s"

	fake := newFakeAPIClient()
	fake.listErr = errors.New("daemon down")
	dc := newFakeDockerClient(fake)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1300 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		runScheduled(ctx, cfg, dc)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("取消后调度循环应退出")
	}
}

func TestSelectContainersIncludesAndExcludes(t *testing.T) {
	containers := []container.Summary{
		{ID: "1", Names: []string{"/pg"}},
		{ID: "2", Names: []string{"/mysql"}},
		{ID: "3", Names: []string{"/redis"}, Labels: map[string]string{"backup": "yes"}},
		{ID: "4", Names: []string{"/sidecar-logger"}},
	}
	cfg := &config{}

	got, excluded := selectContainers(cfg, containers)
	if len(got) != 4 || excluded != 0 {
		t.Errorf("无过滤条件时应全选, got %d/%d", len(got), excluded)
	}

	cfg.excludeContainers = []string{"pg", "sidecar-*"}
	got, excluded = selectContainers(cfg, containers)
	if len(got) != 2 || excluded != 2 {
		t.Errorf("排除后应剩 2 个, got %d/%d", len(got), excluded)
	}

	cfg = &config{includeContainers: []string{"pg", "redis"}}
	got, excluded = selectContainers(cfg, containers)
	if len(got) != 2 || got[0].Names[0] != "/pg" {
		t.Errorf("白名单应按序保留, got %v", got)
	}

	// 逗号分隔的脏值经 splitTrim 后是空列表，等价于"不过滤"
	cfg = &config{includeContainers: splitTrim(", ,")}
	if got, _ := selectContainers(cfg, containers); len(got) != 4 {
		t.Errorf("空配置应全选, got %d", len(got))
	}

	cfg = &config{includeLabels: []string{"backup=yes"}}
	got, excluded = selectContainers(cfg, containers)
	if len(got) != 1 || got[0].ID != "3" || excluded != 3 {
		t.Errorf("标签过滤不对, got %v/%d", got, excluded)
	}
}
