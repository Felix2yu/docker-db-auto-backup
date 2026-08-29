package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
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
	}
}

func setupPostgresFake(fake *fakeAPIClient) string {
	cid := "cid-postgres"
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{Image: "postgres:14"}}
	fake.imageTags["postgres:14"] = []string{"postgres:14"}
	return cid
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

func TestBackupContainerUnsupported(t *testing.T) {
	fake := newFakeAPIClient()
	cid := "cid-nginx"
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{Image: "nginx:latest"}}
	fake.imageTags["nginx:latest"] = []string{"nginx:latest"}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	res, err := backupContainer(context.Background(), cfg, dc, container.Summary{ID: cid, Names: []string{"/nginx"}}, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil {
		t.Errorf("非数据库容器应返回 nil 结果, got %+v", res)
	}
}

func TestBackupContainerPostgres(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	base := t.TempDir()
	res, err := backupContainer(context.Background(), cfg, dc, container.Summary{ID: cid, Names: []string{"/pg"}}, base)
	if err != nil {
		t.Fatalf("backupContainer: %v", err)
	}
	if res == nil || res.name != "pg" || res.providerType != "postgres" {
		t.Fatalf("结果异常: %+v", res)
	}
	matches, _ := filepath.Glob(filepath.Join(base, "pg.sql*"))
	if len(matches) == 0 {
		t.Fatal("应生成 pg.sql 备份文件")
	}
}

func TestBackupContainerSingleDBMode(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	cfg.singleDBMode = true
	base := t.TempDir()
	res, err := backupContainer(context.Background(), cfg, dc, container.Summary{ID: cid, Names: []string{"/pg"}}, base)
	if err != nil {
		t.Fatalf("backupContainer singleDB: %v", err)
	}
	if len(res.dbs) == 0 {
		t.Fatal("单库模式应列出数据库")
	}
	matches, _ := filepath.Glob(filepath.Join(base, "pg", "*.sql*"))
	if len(matches) == 0 {
		t.Fatal("单库模式应在 pg/ 下生成备份文件")
	}
}

func TestBackupContainerMethodError(t *testing.T) {
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
	if _, err := backupContainer(context.Background(), cfg, dc, container.Summary{ID: cid, Names: []string{"/pg"}}, t.TempDir()); err == nil {
		t.Error("备份命令失败应返回错误")
	}
}

func TestWriteBackupEmpty(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		// 备份命令返回空输出
		return nil, nil, 0
	}
	cid := setupPostgresFake(fake)
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)
	base := t.TempDir()
	err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall", "-U", "postgres"},
		filepath.Join(base, "pg.sql"), "sql", "pg (postgres)")
	if err == nil {
		t.Fatal("空备份应返回错误")
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
	err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall", "-U", "postgres"},
		filepath.Join(base, "pg.sql"), "sql", "pg (postgres)")
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
	if err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall", "-U", "postgres"},
		filepath.Join(base, "pg.sql.gz"), "sql", "pg (postgres)"); err != nil {
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
	if err := writeBackup(context.Background(), cfg, dc, cid, []string{"pg_dumpall"},
		filepath.Join(base, "pg.sql"), "sql", "pg"); err == nil {
		t.Fatal("startExec 失败应返回错误")
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
