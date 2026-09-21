package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/schollz/progressbar/v3"
)

func testBackupConfig(t *testing.T) *config {
	t.Helper()
	return &config{
		backupDir:      t.TempDir(),
		compression:    "plain",
		backupValidate: true,
		showProgress:   false,
		puid:           0,
		pgid:           0,
		workers:        1,
	}
}

func setupPostgresFake(fake *fakeAPIClient) string {
	cid := "cid-postgres"
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{Image: "postgres:14"}}
	fake.imageTags["postgres:14"] = []string{"postgres:14"}
	return cid
}

// runBackupOnce 走完整 backup 流程，返回日期目录。
func runBackupOnce(t *testing.T, cfg *config, dc *dockerClient, at time.Time) string {
	t.Helper()
	if err := backup(context.Background(), cfg, dc, at); err != nil {
		t.Fatalf("backup 失败: %v", err)
	}
	return filepath.Join(cfg.backupDir, at.Format("2006-01-02"))
}

func TestContainerName(t *testing.T) {
	c := container.Summary{ID: "abcdef1234567890", Names: []string{"/pg"}}
	if got := containerName(c); got != "pg" {
		t.Errorf("containerName = %q, want pg", got)
	}
	c2 := container.Summary{ID: "abcdef1234567890"}
	if got := containerName(c2); got != "abcdef123456" {
		t.Errorf("containerName = %q, want abcdef123456", got)
	}
}

func TestBuildPlansUnsupported(t *testing.T) {
	fake := newFakeAPIClient()
	cid := "cid-nginx"
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{Image: "nginx:latest"}}
	fake.imageTags["nginx:latest"] = []string{"nginx:latest"}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	plans, skipped, unmatched := buildPlans(context.Background(), cfg, dc,
		[]container.Summary{{ID: cid, Names: []string{"/nginx"}}}, t.TempDir())
	if len(plans) != 0 {
		t.Errorf("非数据库容器不应生成备份计划, got %d", len(plans))
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(unmatched) != 0 {
		t.Errorf("非数据库容器不应进入疑似清单, got %v", unmatched)
	}
}

func TestBuildPlansUnmatchedDatabase(t *testing.T) {
	fake := newFakeAPIClient()
	cid := "cid-mydb"
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{Image: "myorg/postgres-fork:1"}}
	fake.imageTags["myorg/postgres-fork:1"] = []string{"myorg/postgres-fork:1"}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	_, skipped, unmatched := buildPlans(context.Background(), cfg, dc,
		[]container.Summary{{ID: cid, Names: []string{"/mydb"}}}, t.TempDir())
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(unmatched) != 1 {
		t.Fatalf("疑似数据库应被上报, got %v", unmatched)
	}
}

func TestBackupContainerPostgres(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	cid := setupPostgresFake(fake)
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	dir := runBackupOnce(t, cfg, dc, time.Now())

	matches, _ := filepath.Glob(filepath.Join(dir, "pg.sql*"))
	if len(matches) == 0 {
		t.Fatal("应生成 pg.sql 备份文件")
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("应写入备份清单: %v", err)
	}
	if m.Status != statusSuccess || len(m.Containers) != 1 {
		t.Errorf("清单异常: status=%s containers=%d", m.Status, len(m.Containers))
	}
	if m.Containers[0].Mode != modeFull {
		t.Errorf("mode = %q, want full", m.Containers[0].Mode)
	}
}

func TestBackupContainerSingleDBMode(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	cid := setupPostgresFake(fake)
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	cfg.singleDBMode = true
	dir := runBackupOnce(t, cfg, dc, time.Now())

	matches, _ := filepath.Glob(filepath.Join(dir, "pg", "*.sql*"))
	if len(matches) == 0 {
		t.Fatal("单库模式应在 pg/ 下生成备份文件")
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	if m.Containers[0].Mode != modeSingle {
		t.Errorf("mode = %q, want single", m.Containers[0].Mode)
	}
	if len(m.Containers[0].Files) < 3 {
		t.Errorf("单库模式应包含 globals 与各库, got %d", len(m.Containers[0].Files))
	}
}

func TestBuildPlansMethodError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "env" {
			return nil, []byte("err"), 1
		}
		return nil, nil, 0
	}
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	plans, skipped, _ := buildPlans(context.Background(), cfg, dc,
		[]container.Summary{{ID: cid, Names: []string{"/pg"}}}, t.TempDir())
	if len(plans) != 0 {
		t.Errorf("构造备份命令失败时不应生成计划, got %d", len(plans))
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
}

func TestWriteBackupEmpty(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		return nil, nil, 0
	}
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	base := t.TempDir()
	err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall", "-U", "postgres"}, nil,
		filepath.Join(base, "pg.sql"), "sql", "pg (postgres)", false)
	if err == nil {
		t.Fatal("空备份应返回错误")
	}
	if isRetryable(err) {
		t.Error("空备份属于确定性错误，不应重试")
	}
}

func TestWriteBackupValidationFailure(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "pg_dumpall" || cmd[0] == "pg_dump" {
			return []byte("这不是有效的转储\n"), nil, 0
		}
		if cmd[0] == "env" {
			return []byte("POSTGRES_USER=postgres\n"), nil, 0
		}
		return nil, nil, 0
	}
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	base := t.TempDir()
	err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall", "-U", "postgres"}, nil,
		filepath.Join(base, "pg.sql"), "sql", "pg (postgres)", false)
	if err == nil {
		t.Fatal("校验失败应返回错误")
	}
}

func TestWriteBackupWithCompression(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	cfg.compression = "gzip"
	base := t.TempDir()
	if err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall", "-U", "postgres"}, nil,
		filepath.Join(base, "pg.sql.gz"), "sql", "pg (postgres)", false); err != nil {
		t.Fatalf("压缩备份失败: %v", err)
	}
}

func TestWriteBackupStartExecError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		return nil, []byte("exec failed"), 1
	}
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	base := t.TempDir()
	if err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall"}, nil,
		filepath.Join(base, "pg.sql"), "sql", "pg", false); err == nil {
		t.Fatal("导出命令失败应返回错误")
	}
}

// TestWriteBackupNonZeroExit 覆盖 C2：退出码非 0 时，即便输出内容"看起来合法"也必须判失败。
func TestWriteBackupNonZeroExit(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "pg_dumpall" {
			return []byte("-- PostgreSQL database dump\n-- PostgreSQL database dump complete\n"), []byte("permission denied"), 3
		}
		return nil, nil, 0
	}
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	base := t.TempDir()
	err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall"}, nil,
		filepath.Join(base, "pg.sql"), "sql", "pg", false)
	if err == nil {
		t.Fatal("退出码非 0 应判为失败")
	}
	if !strings.Contains(err.Error(), "退出码 3") || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("错误信息应包含退出码与 stderr, got: %v", err)
	}
	if isRetryable(err) {
		t.Error("退出码失败属于确定性错误，不应重试")
	}
}

func TestSelectContainers(t *testing.T) {
	cs := []container.Summary{
		{ID: "1", Names: []string{"/keep"}, Labels: map[string]string{"backup": "true"}},
		{ID: "2", Names: []string{"/drop"}},
		{ID: "3", Names: []string{"/other"}, Labels: map[string]string{"env": "prod"}},
	}
	cfg := &config{excludeContainers: []string{"drop"}}
	got, excluded := selectContainers(cfg, cs)
	if len(got) != 2 || excluded != 1 {
		t.Fatalf("排除规则异常: got=%d excluded=%d", len(got), excluded)
	}

	cfg = &config{includeContainers: []string{"keep"}}
	got, _ = selectContainers(cfg, cs)
	if len(got) != 1 || containerName(got[0]) != "keep" {
		t.Fatalf("白名单规则异常: %+v", got)
	}

	cfg = &config{includeLabels: []string{"backup=true"}}
	got, _ = selectContainers(cfg, cs)
	if len(got) != 1 || containerName(got[0]) != "keep" {
		t.Fatalf("标签规则异常: %+v", got)
	}
}

func TestProgressWriter(t *testing.T) {
	bar := progressbar.NewOptions64(-1, progressbar.OptionSetWriter(os.Stderr), progressbar.OptionShowCount())
	pw := &progressWriter{w: io.Discard, bar: bar}
	n, err := pw.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Errorf("Write 返回 n=%d err=%v, want 5 nil", n, err)
	}
}

func TestCleanOldBackups(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour)
	recent := now.Add(-1 * 24 * time.Hour)
	for _, d := range []time.Time{old, recent} {
		name := d.Format("2006-01-02")
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 放一个无效目录名，不应被删除或 panic
	if err := os.MkdirAll(filepath.Join(dir, "not-a-date"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config{backupDir: dir, retentionDays: 3}
	cleanOldBackups(cfg, now)
	if _, err := os.Stat(filepath.Join(dir, old.Format("2006-01-02"))); !os.IsNotExist(err) {
		t.Error("旧备份应被清理")
	}
	if _, err := os.Stat(filepath.Join(dir, recent.Format("2006-01-02"))); err != nil {
		t.Error("新备份应保留")
	}
	if _, err := os.Stat(filepath.Join(dir, "not-a-date")); err != nil {
		t.Error("无效目录名不应被删除")
	}
}

func TestApplyOwnership(t *testing.T) {
	// puid/pgid 为 0 时不调用 chown
	applyOwnership("/nonexistent-path", &config{puid: 0, pgid: 0})

	// 非零时尝试 chown（非 root 环境会失败但被忽略，仍覆盖分支）
	f := t.TempDir()
	applyOwnership(f, &config{puid: 1, pgid: 1})
}

// TestPartialFailureStillNotifies 覆盖 A1：单个容器失败不应阻断整体流程。
func TestPartialFailureStillNotifies(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "env" {
			return []byte("POSTGRES_USER=postgres\n"), nil, 0
		}
		if cmd[0] == "pg_dumpall" {
			return nil, []byte("boom"), 1
		}
		return nil, nil, 0
	}
	cid := setupPostgresFake(fake)
	fake.containers = []container.Summary{
		{ID: cid, Names: []string{"/pg-broken"}},
	}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	at := time.Now()

	// 只有一个容器且失败：整体判为 failed
	if err := backup(context.Background(), cfg, dc, at); err == nil {
		t.Fatal("全部容器失败时应返回错误")
	}
	dir := filepath.Join(cfg.backupDir, at.Format("2006-01-02"))
	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("失败时也应写入清单: %v", err)
	}
	if m.Status != statusFailed {
		t.Errorf("status = %s, want failed", m.Status)
	}
	if len(m.Failures) == 0 {
		t.Error("清单应记录失败明细")
	}
}

func TestRunLockPreventsConcurrentRun(t *testing.T) {
	dir := t.TempDir()
	l1, err := acquireRunLock(dir, time.Hour)
	if err != nil {
		t.Fatalf("首次获取锁失败: %v", err)
	}
	if _, err := acquireRunLock(dir, time.Hour); err == nil {
		t.Fatal("并发执行时应拒绝第二个实例")
	}
	l1.release()
	if _, err := acquireRunLock(dir, time.Hour); err != nil {
		t.Fatalf("释放后应可再次获取: %v", err)
	}
}
