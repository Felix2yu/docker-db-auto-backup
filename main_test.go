package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

func makePostgresFake() (*fakeAPIClient, string) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	cid := "cid-postgres"
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{Image: "postgres:14"}}
	fake.imageTags["postgres:14"] = []string{"postgres:14"}
	return fake, cid
}

func TestRunScheduledInvalidSchedule(t *testing.T) {
	cfg := &config{schedule: "not-a-cron-expression"}
	runScheduled(context.Background(), cfg, nil)
}

func TestRunScheduledValidCancelled(t *testing.T) {
	cfg := &config{schedule: "* * * * *"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runScheduled(ctx, cfg, nil)
}

func TestBackupNoContainers(t *testing.T) {
	fake := newFakeAPIClient()
	dc := newFakeDockerClient(fake)
	cfg := &config{backupDir: t.TempDir(), compression: "plain", backupValidate: true}
	if err := backup(context.Background(), cfg, dc, time.Now()); err != nil {
		t.Fatalf("无容器时 backup 不应报错: %v", err)
	}
}

func TestBackupListError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.listErr = errFake("list failed")
	dc := newFakeDockerClient(fake)
	cfg := &config{backupDir: t.TempDir(), compression: "plain"}
	if err := backup(context.Background(), cfg, dc, time.Now()); err == nil {
		t.Fatal("listContainers 失败应返回错误")
	}
}

func TestBackupWithContainerAndHealthchecks(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fake, cid := makePostgresFake()
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	dc := newFakeDockerClient(fake)
	cfg := &config{
		backupDir:       t.TempDir(),
		compression:     "plain",
		backupValidate:  true,
		healthchecksURL: srv.URL,
		retentionDays:   3,
	}
	if err := backup(context.Background(), cfg, dc, time.Now()); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if len(hits) == 0 || !strings.HasSuffix(hits[0], "/start") {
		t.Errorf("应首先发送 /start 心跳, hits=%v", hits)
	}
}

func TestBackupWithNotifyError(t *testing.T) {
	fake, cid := makePostgresFake()
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	dc := newFakeDockerClient(fake)
	cfg := &config{
		backupDir:      t.TempDir(),
		compression:    "plain",
		backupValidate: true,
		notifyURLs:     []string{"http://127.0.0.1:1/unreachable"},
	}
	if err := backup(context.Background(), cfg, dc, time.Now()); err != nil {
		t.Fatalf("notify 失败不应影响 backup: %v", err)
	}
}

// TestBackupPartialFailureKeepsGoing 覆盖 A1：
// 一个容器失败时，其余容器的产出仍然保留，整体判为"部分失败"而不是中断。
func TestBackupPartialFailureKeepsGoing(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		joined := strings.Join(cmd, " ")
		switch {
		case cmd[0] == "env":
			return []byte("POSTGRES_USER=postgres\n"), nil, 0
		case strings.Contains(joined, "redis-cli"):
			return nil, []byte("BGSAVE 失败"), 1
		default:
			return dumpHandler(cmd)
		}
	}
	pgID := "cid-postgres"
	fake.inspect[pgID] = container.InspectResponse{Config: &container.Config{Image: "postgres:14"}}
	fake.imageTags["postgres:14"] = []string{"postgres:14"}
	rdID := "cid-redis"
	fake.inspect[rdID] = container.InspectResponse{Config: &container.Config{Image: "redis:7"}}
	fake.imageTags["redis:7"] = []string{"redis:7"}

	fake.containers = []container.Summary{
		{ID: pgID, Names: []string{"/pg"}},
		{ID: rdID, Names: []string{"/rd"}},
	}
	dc := newFakeDockerClient(fake)
	cfg := &config{backupDir: t.TempDir(), compression: "plain", backupValidate: true, workers: 1}
	at := time.Now()

	// 部分失败不应返回 error：成功的产出仍需继续推送与通知。
	if err := backup(context.Background(), cfg, dc, at); err != nil {
		t.Fatalf("部分失败时不应中断整体流程: %v", err)
	}
	dir := filepath.Join(cfg.backupDir, at.Format("2006-01-02"))
	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	if m.Status != statusPartial {
		t.Errorf("status = %s, want partial", m.Status)
	}
	if len(m.Containers) != 1 || m.Containers[0].Name != "pg" {
		t.Errorf("成功的容器应被记录: %+v", m.Containers)
	}
	if len(m.Failures) != 1 {
		t.Errorf("失败明细应有 1 条, got %d", len(m.Failures))
	}
}

func TestBackupKopiaFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fake, cid := makePostgresFake()
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	dc := newFakeDockerClient(fake)
	at := time.Now()
	cfg := &config{
		backupDir:       t.TempDir(),
		compression:     "plain",
		backupValidate:  true,
		healthchecksURL: srv.URL,
		kopia: &kopiaConfig{
			repositoryType: "s3",
			password:       "pw",
			configFile:     filepath.Join(t.TempDir(), "repo.config"),
		},
	}
	// PATH 中无 kopia，ensureRepository 失败。
	// 备份本身是成功的，异地推送失败应降级为"部分失败"并记录，而不是让整轮备份作废。
	if err := backup(context.Background(), cfg, dc, at); err != nil {
		t.Fatalf("Kopia 失败不应让已成功的备份作废: %v", err)
	}
	m, err := readManifest(filepath.Join(cfg.backupDir, at.Format("2006-01-02")))
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	if m.Status != statusPartial {
		t.Errorf("status = %s, want partial", m.Status)
	}
	if m.Kopia == nil || m.Kopia.Pushed {
		t.Error("清单应记录异地快照失败")
	}
}

func TestBackupKopiaSuccess(t *testing.T) {
	writeFakeKopia(t, "kopia", "exit 0")
	fake, cid := makePostgresFake()
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	dc := newFakeDockerClient(fake)
	cfg := &config{
		backupDir:      t.TempDir(),
		compression:    "plain",
		backupValidate: true,
		kopia: &kopiaConfig{
			repositoryType: "s3",
			password:       "pw",
			configFile:     filepath.Join(t.TempDir(), "repo.config"),
		},
	}
	if err := backup(context.Background(), cfg, dc, time.Now()); err != nil {
		t.Fatalf("kopia 成功时 backup 不应报错: %v", err)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
