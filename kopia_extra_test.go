package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeKopia 写一个假的 kopia 可执行文件到临时目录，并返回扩充后的 PATH。
func writeFakeKopia(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+orig)
	return path
}

func TestNewKopiaClientNil(t *testing.T) {
	if k := newKopiaClient(nil); k != nil {
		t.Error("nil cfg 应返回 nil")
	}
	if k := newKopiaClient(&config{}); k != nil {
		t.Error("无 kopia 配置应返回 nil")
	}
	cfg := &config{kopia: &kopiaConfig{repositoryType: "s3"}}
	if k := newKopiaClient(cfg); k == nil {
		t.Error("有 kopia 配置应返回客户端")
	}
}

func TestKopiaRepositoryTypeArg(t *testing.T) {
	k := &kopiaClient{cfg: &kopiaConfig{repositoryType: "posix"}}
	if got := k.repositoryTypeArg(); got != "filesystem" {
		t.Errorf("posix 应映射为 filesystem, got %q", got)
	}
	k2 := &kopiaClient{cfg: &kopiaConfig{repositoryType: "s3"}}
	if got := k2.repositoryTypeArg(); got != "s3" {
		t.Errorf("s3 应保持原样, got %q", got)
	}
}

func TestKopiaRunSuccess(t *testing.T) {
	writeFakeKopia(t, "kopia", "echo kopia-output; exit 0")
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	out, err := k.run(context.Background(), "repository", "status")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "kopia-output\n" {
		t.Errorf("run 输出 = %q", out)
	}
}

func TestKopiaRunFailure(t *testing.T) {
	writeFakeKopia(t, "kopia", "echo something on stderr >&2; exit 3")
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	if _, err := k.run(context.Background(), "repository", "status"); err == nil {
		t.Fatal("非零退出应返回错误")
	}
}

func TestKopiaEnsureRepositoryConnected(t *testing.T) {
	writeFakeKopia(t, "kopia", "exit 0")
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	if err := k.ensureRepository(context.Background()); err != nil {
		t.Fatalf("已连接时不应报错: %v", err)
	}
}

func TestKopiaEnsureRepositoryConnect(t *testing.T) {
	// status 总失败，触发 connect/create 分支
	writeFakeKopia(t, "kopia", `case "$*" in *"repository status"*) exit 1;; esac; exit 0`)
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	if err := k.ensureRepository(context.Background()); err == nil {
		t.Fatal("connect 后 status 仍失败应返回错误")
	}
}

func TestKopiaEnsureRepositoryCreate(t *testing.T) {
	writeFakeKopia(t, "kopia", `case "$*" in *"repository status"*) exit 1;; esac; exit 0`)
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw", createRepository: true}}
	if err := k.ensureRepository(context.Background()); err == nil {
		t.Fatal("create 后 status 仍失败应返回错误")
	}
}

func TestKopiaEnsurePolicy(t *testing.T) {
	writeFakeKopia(t, "kopia", "exit 0")
	// 空 policyCompression 直接返回
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	if err := k.ensurePolicy(context.Background()); err != nil {
		t.Fatalf("空 policyCompression 不应报错: %v", err)
	}
	// 设置 compression，成功路径
	k2 := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw", policyCompression: "gzip"}}
	if err := k2.ensurePolicy(context.Background()); err != nil {
		t.Fatalf("ensurePolicy: %v", err)
	}
}

func TestKopiaEnsurePolicyFailure(t *testing.T) {
	writeFakeKopia(t, "kopia", "exit 5")
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw", policyCompression: "gzip"}}
	if err := k.ensurePolicy(context.Background()); err == nil {
		t.Fatal("策略设置失败应返回错误")
	}
}

func TestKopiaSnapshotCreate(t *testing.T) {
	writeFakeKopia(t, "kopia", "exit 0")
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	if err := k.snapshotCreate(context.Background(), "/backups/2026-01-02"); err != nil {
		t.Fatalf("snapshotCreate: %v", err)
	}
}

func TestKopiaSnapshotCreateFailure(t *testing.T) {
	writeFakeKopia(t, "kopia", "exit 7")
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	if err := k.snapshotCreate(context.Background(), "/backups/2026-01-02"); err == nil {
		t.Fatal("快照创建失败应返回错误")
	}
}
