package main

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestListContainers(t *testing.T) {
	fake := newFakeAPIClient()
	fake.containers = []container.Summary{
		{ID: "abc123", Names: []string{"/pg"}},
		{ID: "def456", Names: []string{"/mysql"}},
	}
	dc := newFakeDockerClient(fake)
	got, err := dc.listContainers(context.Background())
	if err != nil {
		t.Fatalf("listContainers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 containers, got %d", len(got))
	}
}

func TestContainerImageNames(t *testing.T) {
	fake := newFakeAPIClient()
	fake.inspect["c1"] = container.InspectResponse{
		Config: &container.Config{Image: "postgres:14"},
	}
	fake.imageTags["postgres:14"] = []string{"postgres:14", "postgres:latest"}
	dc := newFakeDockerClient(fake)
	names, err := dc.containerImageNames(context.Background(), "c1")
	if err != nil {
		t.Fatalf("containerImageNames: %v", err)
	}
	if len(names) != 1 || names[0] != "postgres" {
		t.Fatalf("want [postgres], got %v", names)
	}

	// 第二次应走缓存
	names2, err := dc.containerImageNames(context.Background(), "c1")
	if err != nil || len(names2) != 1 {
		t.Fatalf("cached lookup failed: %v %v", names2, err)
	}
}

func TestContainerEnv(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	env, err := dc.containerEnv(context.Background(), "c1")
	if err != nil {
		t.Fatalf("containerEnv: %v", err)
	}
	if env["POSTGRES_USER"] != "postgres" {
		t.Errorf("POSTGRES_USER = %q, want postgres", env["POSTGRES_USER"])
	}

	// 缓存命中
	env2, err := dc.containerEnv(context.Background(), "c1")
	if err != nil || env2["POSTGRES_USER"] != "postgres" {
		t.Fatalf("cached env lookup failed")
	}
}

func TestContainerEnvExecError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		return nil, []byte("boom"), 1
	}
	dc := newFakeDockerClient(fake)
	if _, err := dc.containerEnv(context.Background(), "c1"); err == nil {
		t.Fatal("expected error from failed exec")
	}
}

func TestHasBinary(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	ok, err := dc.hasBinary(context.Background(), "c1", "mariadb-dump")
	if err != nil {
		t.Fatalf("hasBinary: %v", err)
	}
	if ok {
		t.Error("mariadb-dump 应判定为不存在")
	}

	// 让 which 返回成功
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "which" {
			return []byte("/usr/bin/x"), nil, 0
		}
		return nil, nil, 0
	}
	ok2, err := dc.hasBinary(context.Background(), "c1", "redis-cli")
	if err != nil || !ok2 {
		t.Fatalf("redis-cli 应判定为存在: ok=%v err=%v", ok2, err)
	}

	// binCache 命中
	if ok3, _ := dc.hasBinary(context.Background(), "c1", "redis-cli"); !ok3 {
		t.Error("binCache 命中应返回 true")
	}
}

func TestExecCollect(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	out, err := dc.execCollect(context.Background(), "c1", []string{"env"}, nil)
	if err != nil {
		t.Fatalf("execCollect: %v", err)
	}
	if len(out) == 0 {
		t.Error("execCollect 应返回输出")
	}
}

func TestExecCollectError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		return nil, []byte("fail"), 2
	}
	dc := newFakeDockerClient(fake)
	if _, err := dc.execCollect(context.Background(), "c1", []string{"env"}, nil); err == nil {
		t.Fatal("期望非零退出码返回错误")
	}
}

func TestStartExec(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	id, attach, err := dc.startExec(context.Background(), "c1", []string{"pg_dumpall"}, nil)
	if err != nil {
		t.Fatalf("startExec: %v", err)
	}
	if id == "" {
		t.Error("exec id 不应为空")
	}
	if attach.Reader == nil {
		t.Error("attach reader 不应为空")
	}
	attach.Close()
}

func TestStartExecCloses(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	id, attach, err := dc.startExec(context.Background(), "c1", []string{"pg_dumpall"}, nil)
	if err != nil {
		t.Fatalf("startExec: %v", err)
	}
	if id == "" {
		t.Error("exec id 不应为空")
	}
	if attach.Reader == nil {
		t.Error("attach reader 不应为空")
	}
	attach.Close()
}
