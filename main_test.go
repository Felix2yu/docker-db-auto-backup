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

func TestBackupWithShoutrrrError(t *testing.T) {
	fake, cid := makePostgresFake()
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/pg"}}}
	dc := newFakeDockerClient(fake)
	cfg := &config{
		backupDir:      t.TempDir(),
		compression:    "plain",
		backupValidate: true,
		shoutrrrURLs:   []string{"http://127.0.0.1:1/unreachable"},
	}
	if err := backup(context.Background(), cfg, dc, time.Now()); err != nil {
		t.Fatalf("shoutrrr 失败不应影响 backup: %v", err)
	}
}

func TestBackupContainerError(t *testing.T) {
	fake, cidOK := makePostgresFake()
	// 第二个容器没有 inspect 数据，会导致 backupContainer 失败
	fake.containers = []container.Summary{
		{ID: cidOK, Names: []string{"/pg"}},
		{ID: "cid-broken", Names: []string{"/broken"}},
	}
	dc := newFakeDockerClient(fake)
	cfg := &config{backupDir: t.TempDir(), compression: "plain", backupValidate: true}
	if err := backup(context.Background(), cfg, dc, time.Now()); err == nil {
		t.Fatal("存在容器备份失败时应返回错误")
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
	// PATH 中无 kopia，ensureRepository 失败，应返回错误
	if err := backup(context.Background(), cfg, dc, time.Now()); err == nil {
		t.Fatal("kopia 仓库失败应返回错误")
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
