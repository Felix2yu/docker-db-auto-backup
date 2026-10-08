package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckFreeSpaceBranches(t *testing.T) {
	dir := t.TempDir()

	// 路径不存在时 statfs 失败：只跳过预检，不能拦住备份
	if err := checkFreeSpace(filepath.Join(dir, "missing"), 1<<20, 1<<20); err != nil {
		t.Errorf("空间检查不可用时应放行: %v", err)
	}
	// 没有下限要求时直接通过
	if err := checkFreeSpace(dir, 0, 0); err != nil {
		t.Errorf("无要求时应通过: %v", err)
	}
	// 要求高于可用空间（int64 上限）时必须失败
	err := checkFreeSpace(dir, int64(1)<<62, 0)
	if err == nil || !strings.Contains(err.Error(), "可用空间不足") {
		t.Errorf("got %v", err)
	}
}

func TestRequiredSpaceFor(t *testing.T) {
	if got := requiredSpaceFor(nil, 500); got != 500 {
		t.Errorf("无历史清单时应回落到下限, got %d", got)
	}
	prev := &backupManifest{Containers: []containerManifest{{Files: []fileEntry{{Size: 1000}}}}}
	if got := requiredSpaceFor(prev, 100); got != 1200 {
		t.Errorf("应按历史体积留 20%% 余量, got %d", got)
	}
	if got := requiredSpaceFor(prev, 5000); got != 5000 {
		t.Errorf("估算低于下限时应取下限, got %d", got)
	}
}

func TestCleanupStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	// 目录不存在时静默返回即可，不应 panic
	cleanupStaleTempFiles(filepath.Join(dir, "no-such-dir"))

	sub := filepath.Join(dir, ".tmp")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".auto-backup-aaa", ".auto-backup-bbb"} {
		if err := os.WriteFile(filepath.Join(sub, name), []byte("半成品"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cleanupStaleTempFiles(sub)

	entries, err := os.ReadDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Errorf("应清掉残留文件并保留子目录, got %v", entries)
	}
}

func TestRunLockStaleTakeover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lockFileName)
	if err := os.WriteFile(path, []byte("pid=1 started=long-ago"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	l, err := acquireRunLock(dir, time.Hour)
	if err != nil {
		t.Fatalf("僵死锁应被接管: %v", err)
	}
	if !l.held {
		t.Error("接管后应持有锁")
	}
	l.release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("释放后锁文件应消失")
	}
	// 重复释放不应报错
	l.release()
}

func TestRunLockFailurePaths(t *testing.T) {
	// backupDir 为空时不做加锁（单次运行模式的临时目录场景）
	l, err := acquireRunLock("", time.Hour)
	if err != nil || l == nil || l.held {
		t.Errorf("空目录应直接放行, got %v %v", l, err)
	}
	l.release()

	// 锁目录本身不可创建时返回错误而不是 panic
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireRunLock(filepath.Join(blocker, "sub"), time.Hour); err == nil {
		t.Error("无法创建锁文件时应返回错误")
	}

	// 未持有时 release 是空操作
	(&runLock{}).release()
	(&runLock{path: "x", held: false}).release()

	// 删除失败只记日志
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	(&runLock{path: dir, held: true}).release()
}

func TestAcquireRunLockFreshLockBlocks(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireRunLock(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()

	// 锁文件里的 pid 只用于诊断，判断僵死只看 mtime
	second, err := acquireRunLock(dir, time.Hour)
	if err == nil {
		second.release()
		t.Fatal("同一时间只应有一个实例持有锁")
	}
	if !strings.Contains(err.Error(), "已有备份实例正在运行") {
		t.Errorf("got %v", err)
	}
}

func TestListBackupDatesAndManifestErrors(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"2026-09-20", "2026-09-21", "not-a-date", ".kopia"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-09-22"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &backupManifest{Version: 1, Date: "2026-09-21", Status: statusSuccess}
	if err := writeManifest(filepath.Join(dir, "2026-09-21"), m); err != nil {
		t.Fatal(err)
	}

	dates, err := listBackupDates(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dates) != 2 || dates[0] != "2026-09-20" || dates[1] != "2026-09-21" {
		t.Errorf("应只列出日期目录并升序排列, got %v", dates)
	}
	if _, err := listBackupDates(filepath.Join(dir, "missing")); err == nil {
		t.Error("目录不存在时应返回错误")
	}

	// 没有清单的日期目录：findPreviousManifest 要跳过并继续往前找
	prev, err := findPreviousManifest(dir, "2026-09-23")
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.Date != "2026-09-21" {
		t.Errorf("应找到最近一份有清单的备份, got %+v", prev)
	}
	if got, err := findPreviousManifest(filepath.Join(dir, "missing"), "2026-09-23"); err == nil || got != nil {
		t.Errorf("目录不可读时应返回错误: %v %v", got, err)
	}
	// 全是未来/同名日期时返回 nil 而不是错误
	if got, err := findPreviousManifest(dir, "2026-01-01"); err != nil || got != nil {
		t.Errorf("got %+v, %v", got, err)
	}

	if _, err := readManifest(filepath.Join(dir, "not-a-date")); err == nil {
		t.Error("缺清单时应报错")
	}
	if err := writeManifest(filepath.Join(dir, "missing-dir"), m); err == nil {
		t.Error("目录不存在时写入应报错")
	}
	if _, err := fileChecksum(dir); err == nil {
		t.Error("对目录算校验和应报错")
	}
}

func TestDetectAnomaliesMoreCases(t *testing.T) {
	prev := &backupManifest{Containers: []containerManifest{
		{Name: "pg", Files: []fileEntry{{Size: 1000}}},
		{Name: "old", Files: []fileEntry{{Size: 1}}},
	}}

	// 体积暴涨超过 4 倍阈值，同时点名上次存在、本次缺失的容器
	growth := &backupManifest{Containers: []containerManifest{{Name: "pg", Files: []fileEntry{{Size: 9000}}}}}
	joined := strings.Join(detectAnomalies(prev, growth, 0.5), "\n")
	for _, want := range []string{"增长", "old", "降至"} {
		if !strings.Contains(joined, want) {
			t.Errorf("应报出 %q:\n%s", want, joined)
		}
	}

	// prev 里体积为 0 的容器算不出比例，应被跳过而不是除零
	zeroPrev := &backupManifest{Containers: []containerManifest{
		{Name: "pg", Files: []fileEntry{{Size: 1000}}},
		{Name: "zero", Files: []fileEntry{{Size: 0}}},
	}}
	zeroCur := &backupManifest{Containers: []containerManifest{
		{Name: "pg", Files: []fileEntry{{Size: 9000}}},
		{Name: "zero", Files: []fileEntry{{Size: 9000}}},
	}}
	got := detectAnomalies(zeroPrev, zeroCur, 0)
	if len(got) != 1 || !strings.Contains(got[0], "pg") {
		t.Errorf("driftRatio<=0 应回退默认阈值，且体积为 0 的基线不比比例, got %v", got)
	}

	// 上次为空清单时无基线可比
	empty := &backupManifest{}
	if got := detectAnomalies(empty, growth, 0.5); got != nil {
		t.Errorf("无基线时不应告警, got %v", got)
	}
	if got := detectAnomalies(nil, growth, 0.5); got != nil {
		t.Errorf("prev 为 nil 时应直接返回, got %v", got)
	}

	// 体积骤降与容器缺失可以同时出现
	drop := &backupManifest{Containers: []containerManifest{
		{Name: "pg", Files: []fileEntry{{Size: 400}}},
		{Name: "other"},
	}}
	base := &backupManifest{Containers: []containerManifest{
		{Name: "pg", Files: []fileEntry{{Size: 1000}}},
		{Name: "gone", Files: []fileEntry{{Size: 1}}},
	}}
	joined = strings.Join(detectAnomalies(base, drop, 0.5), "\n")
	if !strings.Contains(joined, "gone") || !strings.Contains(joined, "下降") {
		t.Errorf("应同时报出缺失与体积下降: %s", joined)
	}
}

func TestRunCollectorAddWarningAndKopiaInfo(t *testing.T) {
	rc := newRunCollector("2026-09-21", time.Now())
	rc.addWarning("注意")
	rc.setStatus(statusPartial)
	rc.setKopia(&kopiaManifestInfo{Pushed: true, SnapshotID: "abc"})
	m := rc.finish(time.Second, "Local")
	if len(m.Warnings) != 1 || m.Status != statusPartial || m.Kopia == nil {
		t.Errorf("got %+v", m)
	}
	if m.Timezone != "Local" || m.DurationSeconds != 1 {
		t.Errorf("finish 应写入耗时与时区: %+v", m)
	}
	if size, ok := m.containerSize("不存在"); ok || size != 0 {
		t.Errorf("未知容器应报未找到, got %d %v", size, ok)
	}
}

func TestManifestSizeAndCounts(t *testing.T) {
	m := &backupManifest{Containers: []containerManifest{
		{Name: "pg", Files: []fileEntry{{Size: 10}, {Size: 5, System: true}}},
		{Name: "rd"},
	}}
	if m.fileCount() != 2 {
		t.Errorf("fileCount = %d", m.fileCount())
	}
	if m.totalBytes() != 15 {
		t.Errorf("totalBytes = %d", m.totalBytes())
	}
	if size, ok := m.containerSize("pg"); !ok || size != 15 {
		t.Errorf("containerSize(pg) = %d %v", size, ok)
	}
}

func TestManifestVersionMismatchIsTolerated(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":99,"date":"2030-01-01","status":"success","containers":[{"name":"pg","files":[{"path":"a.sql","size":1}]}]}`
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("未来版本的清单应仍可读取（字段向前兼容）: %v", err)
	}
	if m.Version != 99 || m.fileCount() != 1 {
		t.Errorf("got %+v", m)
	}
	if _, err := readManifest(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("目录不存在时应返回 NotExist, got %v", err)
	}
}
