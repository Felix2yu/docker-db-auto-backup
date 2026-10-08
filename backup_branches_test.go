package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

func TestPermanentErrorAndRetryability(t *testing.T) {
	inner := errors.New("退出码 2")
	err := permanent(inner)
	if err.Error() != inner.Error() {
		t.Errorf("Error() 应透传原因, got %q", err.Error())
	}
	if !errors.Is(err, inner) {
		t.Error("Unwrap 后应能按原因判定")
	}
	if isRetryable(err) {
		t.Error("确定性错误不应重试")
	}
	if isRetryable(nil) {
		t.Error("没有错误时不应重试")
	}
	if !isRetryable(errors.New("连接中断")) {
		t.Error("未标记的错误应可重试")
	}
}

func TestMatchesAnyAndLabels(t *testing.T) {
	if matchesAny("pg", []string{"", ""}) {
		t.Error("空模式不应命中")
	}
	if !matchesAny("pg-primary", []string{"pg-*"}) {
		t.Error("通配应命中")
	}
	if !matchesAny("pg", []string{"pg"}) {
		t.Error("同名应命中")
	}
	// 非法 glob：filepath.Match 报错后仍要按精确匹配兜底
	if !matchesAny("[weird", []string{"[weird"}) {
		t.Error("含非法通配符的名字应按字面匹配")
	}

	labels := map[string]string{"backup": "yes", "empty": ""}
	if hasAnyLabel(labels, []string{"", "  "}) {
		t.Error("空选择器应被忽略")
	}
	if !hasAnyLabel(labels, []string{"backup=yes"}) {
		t.Error("key=value 应命中")
	}
	if !hasAnyLabel(labels, []string{"empty="}) {
		t.Error("只写 key= 时应按存在判定")
	}
	if hasAnyLabel(labels, []string{"backup=no"}) {
		t.Error("值不符不应命中")
	}
	if hasAnyLabel(labels, []string{"missing"}) {
		t.Error("不存在的标签不应命中")
	}
	if !hasAnyLabel(labels, []string{"missing", "backup"}) {
		t.Error("任一命中即可")
	}
}

func TestContainerNameEdgeCases(t *testing.T) {
	if got := containerName(container.Summary{ID: "short"}); got != "short" {
		t.Errorf("ID 过短时应原样返回, got %q", got)
	}
}

func TestRelativeToAndFormatDuration(t *testing.T) {
	if got := relativeTo("/data/2026-09-21/pg.sql", "/data"); got != filepath.Join("2026-09-21", "pg.sql") {
		t.Errorf("got %q", got)
	}
	if got := relativeTo("relative.sql", "/abs/base"); got != "relative.sql" {
		t.Errorf("无法求相对路径时应原样返回, got %q", got)
	}
	if got := formatDuration(45 * time.Second); !strings.Contains(got, "秒") {
		t.Errorf("got %q", got)
	}
	if got := formatDuration(2*time.Minute + 5*time.Second); got != "2 分钟 5 秒" {
		t.Errorf("got %q", got)
	}
}

func TestWriteManifestBestEffortFailure(t *testing.T) {
	cfg := &config{backupDir: t.TempDir()}
	// 目录不存在时写入失败，只应记日志而不 panic
	writeManifestBestEffort(filepath.Join(cfg.backupDir, "missing"), &backupManifest{Version: 1}, cfg)
}

// backup() 的每一条提前返回路径都要上报心跳并留下清单，不能静默失败。
func TestBackupEarlyReturns(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 9, 21, 3, 0, 0, 0, time.Local)

	t.Run("备份目录不可创建", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := testBackupConfig(t)
		cfg.backupDir = filepath.Join(blocker, "backups")
		dc := newFakeDockerClient(newFakeAPIClient())

		if err := backup(ctx, cfg, dc, at); err == nil {
			t.Error("备份目录建不出来时应返回错误")
		}
	})

	t.Run("已有实例在运行", func(t *testing.T) {
		cfg := testBackupConfig(t)
		if err := os.MkdirAll(cfg.backupDir, 0o755); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(cfg.backupDir, lockFileName)
		if err := os.WriteFile(lock, []byte("pid=1"), 0o644); err != nil {
			t.Fatal(err)
		}
		dc := newFakeDockerClient(newFakeAPIClient())

		if err := backup(ctx, cfg, dc, at); err == nil {
			t.Error("锁被占用时应跳过本轮")
		}
		// 跳过时不应误报心跳，也不该把锁删掉
		if _, err := os.Stat(lock); err != nil {
			t.Errorf("别人的锁不应被删除: %v", err)
		}
	})

	t.Run("日期目录被文件占位", func(t *testing.T) {
		cfg := testBackupConfig(t)
		if err := os.MkdirAll(cfg.backupDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.backupDir, at.Format("2006-01-02")), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		dc := newFakeDockerClient(newFakeAPIClient())
		if err := backup(ctx, cfg, dc, at); err == nil {
			t.Error("日期目录无法创建时应返回错误")
		}
	})

	t.Run("临时目录被文件占位", func(t *testing.T) {
		cfg := testBackupConfig(t)
		if err := os.MkdirAll(cfg.backupDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.tmpDir(), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		dc := newFakeDockerClient(newFakeAPIClient())
		if err := backup(ctx, cfg, dc, at); err == nil {
			t.Error("临时目录不可用时时应返回错误")
		}
	})

	t.Run("磁盘空间不足", func(t *testing.T) {
		cfg := testBackupConfig(t)
		cfg.minFreeBytes = 1 << 62
		fake := newFakeAPIClient()
		cid := setupPostgresFake(fake)
		fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
		fake.execHandler = dumpHandler

		err := backup(ctx, cfg, newFakeDockerClient(fake), at)
		if err == nil {
			t.Fatal("空间不足时应提前失败")
		}
		if !strings.Contains(err.Error(), "可用空间不足") {
			t.Errorf("got %v", err)
		}
		// 失败也要留下清单，便于 status/list 看到这一轮
		m, readErr := readManifest(filepath.Join(cfg.backupDir, at.Format("2006-01-02")))
		if readErr != nil {
			t.Fatalf("应写入清单: %v", readErr)
		}
		if m.Status != statusFailed || len(m.Failures) == 0 {
			t.Errorf("清单应记录失败状态与原因: %+v", m)
		}
	})
}

// 备份范围之外还要能发现"上次有、这次没了"，以及单库模式回退。
func TestBackupAnomalyAndSingleModeFallback(t *testing.T) {
	ctx := context.Background()

	fake := newFakeAPIClient()
	cid := setupPostgresFake(fake)
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)

	cfg := testBackupConfig(t)
	runBackupOnce(t, cfg, dc, time.Date(2026, 9, 20, 3, 0, 0, 0, time.Local))

	// 第二天容器不再出现：应报缺失与容器数下降两条警告
	fake.containers = nil
	at := time.Date(2026, 9, 21, 3, 0, 0, 0, time.Local)
	if err := backup(ctx, cfg, dc, at); err != nil {
		t.Fatalf("backup: %v", err)
	}
	m, err := readManifest(filepath.Join(cfg.backupDir, "2026-09-21"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Warnings) < 2 {
		t.Errorf("应报出容器缺失与数量下降, got %v", m.Warnings)
	}
	if !strings.Contains(strings.Join(m.Warnings, "\n"), "pg") {
		t.Errorf("警告里应点名 pg: %v", m.Warnings)
	}
}

func TestBackupSingleModeFallbackAndBadDBName(t *testing.T) {
	at := time.Date(2026, 9, 21, 3, 0, 0, 0, time.Local)

	fake := newFakeAPIClient()
	cid := setupPostgresFake(fake)
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		joined := strings.Join(cmd, " ")
		switch {
		case cmd[0] == "env":
			return []byte("POSTGRES_USER=postgres\n"), nil, 0
		case strings.Contains(joined, "psql"):
			// 单库枚举失败 → 回退全库
			return nil, []byte("could not connect to server"), 1
		}
		return dumpHandler(cmd)
	}
	cfg := testBackupConfig(t)
	cfg.singleDBMode = true

	base := runBackupOnce(t, cfg, newFakeDockerClient(fake), at)
	m, err := readManifest(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Containers) != 1 || m.Containers[0].Mode != modeFullFallback {
		t.Fatalf("应记录回退模式: %+v", m.Containers)
	}
	if !strings.Contains(strings.Join(m.Warnings, "\n"), "单库模式回退") {
		t.Errorf("回退应出现在警告里: %v", m.Warnings)
	}
	// 回退后走全库文件：{容器名}.sql
	if _, err := os.Stat(filepath.Join(base, "pg.sql")); err != nil {
		t.Errorf("回退后应产出全库文件: %v", err)
	}
}

func TestCollectTasksSkipsUnsafeDBName(t *testing.T) {
	plan := &containerPlan{
		name:      "pg",
		provider:  providerByName("postgres"),
		mode:      modeSingle,
		dbDir:     "/backups/2026-09-21/pg",
		backupDir: "/backups/2026-09-21",
		dbs: []database{
			{name: "../escape"},
			{name: "ok_db"},
		},
	}
	tasks := collectTasks(&config{compression: "plain"}, []*containerPlan{plan})
	if len(tasks) != 1 {
		t.Fatalf("不安全的库名应被跳过, got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].db.name != "ok_db" || strings.Contains(tasks[0].filePath, "..") {
		t.Errorf("got %+v", tasks[0])
	}
}

// 可重试错误要真的重试，取消则要立刻停下而不是继续退避等待。
func TestRunTaskWithRetry(t *testing.T) {
	newPlan := func() *containerPlan {
		return &containerPlan{
			c:        container.Summary{ID: "c1"},
			name:     "pg",
			provider: providerByName("postgres"),
			mode:     modeFull,
			command:  []string{"pg_dumpall", "-U", "postgres"},
			execEnv:  []string{"PGPASSWORD=secret"},
		}
	}
	task := backupTask{plan: newPlan(), filePath: filepath.Join(t.TempDir(), "pg.sql"), fileExt: "sql", desc: "pg (postgres)"}

	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	fake.execInspectErr = errors.New("exec inspect unavailable")
	dc := newFakeDockerClient(fake)

	cfg := testBackupConfig(t)
	cfg.backupDir = t.TempDir()
	cfg.backupRetries = 1
	cfg.retryBackoff = time.Millisecond
	cfg.backupTimeout = 0

	attempts := 0
	seq := fake.execHandler
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if len(cmd) > 0 && cmd[0] == "pg_dumpall" {
			attempts++
		}
		return seq(cmd)
	}

	if err := runTaskWithRetry(context.Background(), cfg, dc, task, false); err == nil {
		t.Error("ExecInspect 一直失败时应返回错误")
	}
	if attempts != 2 {
		t.Errorf("retries=1 应共尝试 2 次, got %d", attempts)
	}

	// 取消后不应再退避等待
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg.backupRetries = 3
	cfg.retryBackoff = time.Hour
	start := time.Now()
	if err := runTaskWithRetry(ctx, cfg, dc, task, false); err == nil {
		t.Error("取消时应返回错误")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("取消后仍在退避等待: %s", time.Since(start))
	}

	// 负数重试着样要归一为"至少一次"
	fake.execInspectErr = nil
	cfg.backupRetries = -1
	cfg.backupTimeout = time.Minute
	if err := runTaskWithRetry(context.Background(), cfg, dc, task, false); err != nil {
		t.Errorf("应至少成功一次: %v", err)
	}
}

func TestRunTasksWorkersClampedAndEmpty(t *testing.T) {
	cfg := testBackupConfig(t)
	cfg.workers = 8
	cfg.backupDir = t.TempDir()

	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	fake.inspect["c1"] = container.InspectResponse{Config: &container.Config{Image: "postgres:14"}}
	fake.imageTags["postgres:14"] = []string{"postgres:14"}
	dc := newFakeDockerClient(fake)

	plan := &containerPlan{
		c:         container.Summary{ID: "c1"},
		name:      "pg",
		provider:  providerByName("postgres"),
		mode:      modeFull,
		backupDir: cfg.backupDir,
		command:   []string{"pg_dumpall"},
	}
	results, failures := runTasks(context.Background(), cfg, dc, collectTasks(cfg, []*containerPlan{plan}))
	if len(failures) != 0 {
		t.Fatalf("不应失败: %v", failures)
	}
	if len(results) != 1 || len(results[0].Files) != 1 {
		t.Fatalf("got %+v", results)
	}
	if results[0].DurationSeconds <= 0 {
		t.Error("应记录耗时")
	}

	// 没有任务时不应启动任何 worker
	if got, fails := runTasks(context.Background(), cfg, dc, nil); got != nil || fails != nil {
		t.Errorf("空任务列表应返回空: %v %v", got, fails)
	}
}
