package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
	writeFakeKopia(t, "kopia", `echo '{"id":"snapshot-abc"}'`)
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	id, err := k.snapshotCreate(context.Background(), "/backups/2026-01-02", "auto-backup 2026-01-02")
	if err != nil {
		t.Fatalf("snapshotCreate: %v", err)
	}
	if id != "snapshot-abc" {
		t.Errorf("snapshot id = %q, want snapshot-abc", id)
	}
}

func TestKopiaSnapshotCreateFailure(t *testing.T) {
	writeFakeKopia(t, "kopia", "exit 7")
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k", password: "pw"}}
	if _, err := k.snapshotCreate(context.Background(), "/backups/2026-01-02", "desc"); err == nil {
		t.Fatal("快照创建失败应返回错误")
	}
}

func TestParseSnapshotID(t *testing.T) {
	if got := parseSnapshotID("progress...\n{\"id\":\"xyz\"}\n"); got != "xyz" {
		t.Errorf("parseSnapshotID = %q, want xyz", got)
	}
	if got := parseSnapshotID(""); got != "" {
		t.Errorf("空输入应返回空 ID, got %q", got)
	}
	if got := parseSnapshotID("not json"); got != "" {
		t.Errorf("非 JSON 应返回空 ID, got %q", got)
	}
}

func TestKopiaMaintenanceSkipped(t *testing.T) {
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/k/repository.config", maintenanceInterval: 0}}
	if err := k.maybeMaintenance(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("间隔为 0 时应跳过维护: %v", err)
	}
}

func TestKopiaMaintenanceRun(t *testing.T) {
	writeFakeKopia(t, "kopia", "exit 0")
	dir := t.TempDir()
	k := &kopiaClient{cfg: &kopiaConfig{
		configFile:          filepath.Join(dir, ".kopia", "repository.config"),
		maintenanceInterval: time.Hour,
	}}
	if err := k.maybeMaintenance(context.Background(), dir); err != nil {
		t.Fatalf("维护执行失败: %v", err)
	}
	// 立即再次调用应因间隔未到而跳过
	if err := k.maybeMaintenance(context.Background(), dir); err != nil {
		t.Fatalf("间隔内不应重复维护: %v", err)
	}
}

func TestKopiaPolicyArgsWithRetention(t *testing.T) {
	k := &kopiaClient{cfg: &kopiaConfig{
		policyCompression: "zstd",
		retentionFlags:    "--keep-daily=7 --keep-weekly=4",
	}}
	got := k.policyArgs()
	want := []string{"policy", "set", "--global", "--compression", "zstd",
		"--keep-daily=7", "--keep-weekly=4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("policyArgs = %v, want %v", got, want)
	}
}

func TestKopiaEnsureRepositoryVerificationFails(t *testing.T) {
	// status 一直失败、connect 成功：连接后仍要复检，否则视为失败。
	// run() 会把 --config-file 排在最前面，所以按整条命令行匹配而不是位置参数。
	writeFakeKopia(t, "kopia", `
case "$*" in
  *"repository connect"*) exit 0 ;;
  *"repository status"*) echo "not connected" >&2; exit 1 ;;
esac
exit 0`)
	k := &kopiaClient{cfg: &kopiaConfig{
		repositoryType:  "filesystem",
		repositoryFlags: "--path=/tmp/repo",
		configFile:      "/tmp/kopia.cfg",
	}}
	err := k.ensureRepository(context.Background())
	if err == nil || !strings.Contains(err.Error(), "仓库校验失败") {
		t.Errorf("连接后复检失败应上报, got %v", err)
	}
}

// 既没配压缩策略也没配保留策略时，不应为了空参数调用一次 kopia。
func TestKopiaEnsurePolicySkippedWithoutConfig(t *testing.T) {
	writeFakeKopia(t, "kopia", "echo \"被调用了\" >&2; exit 1")
	k := &kopiaClient{cfg: &kopiaConfig{configFile: "/tmp/kopia.cfg"}}
	if err := k.ensurePolicy(context.Background()); err != nil {
		t.Errorf("无策略时应跳过, got %v", err)
	}
}

func TestKopiaMaintenanceFailure(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "repository.config")
	writeFakeKopia(t, "kopia", `
case "$*" in
  *"maintenance run"*) echo "maintenance aborted" >&2; exit 1 ;;
esac
exit 0`)
	k := &kopiaClient{cfg: &kopiaConfig{configFile: cfgFile, maintenanceInterval: time.Minute}}
	err := k.maybeMaintenance(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "maintenance aborted") {
		t.Errorf("维护失败应上报, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".last-maintenance")); err == nil {
		t.Error("维护失败时不应推进维护标记")
	}
}

func TestRunKopiaPolicyFailureMarksCollector(t *testing.T) {
	writeFakeKopia(t, "kopia", `
case "$*" in
  *"policy set"*) echo "denied" >&2; exit 1 ;;
esac
exit 0`)
	dir := t.TempDir()
	cfg := &config{backupDir: dir, kopia: &kopiaConfig{
		repositoryType: "filesystem", configFile: filepath.Join(dir, ".kopia", "repository.config"),
		policyCompression: "zstd",
	}}
	rc := newRunCollector("2026-09-21", time.Now())
	err := runKopia(context.Background(), cfg, dir, "2026-09-21", rc)
	if err == nil {
		t.Fatal("策略失败应返回错误")
	}
	m := rc.finish(time.Second, "Local")
	if m.Kopia == nil || m.Kopia.Pushed {
		t.Errorf("清单应记录异地推送失败: %+v", m.Kopia)
	}
}

func TestRunKopiaMaintenanceFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "repository.config")
	writeFakeKopia(t, "kopia", `
case "$*" in
  *"maintenance run"*) echo "maintenance aborted" >&2; exit 1 ;;
  *"snapshot create"*) echo '{"id":"snap-1"}'; exit 0 ;;
esac
exit 0`)
	cfg := &config{backupDir: dir, kopia: &kopiaConfig{
		repositoryType: "filesystem", configFile: cfgFile, maintenanceInterval: time.Minute,
	}}
	rc := newRunCollector("2026-09-21", time.Now())
	if err := runKopia(context.Background(), cfg, dir, "2026-09-21", rc); err != nil {
		t.Errorf("维护失败不应让整轮异地备份失败: %v", err)
	}
	if m := rc.finish(time.Second, "Local"); m.Kopia == nil || !m.Kopia.Pushed || m.Kopia.SnapshotID != "snap-1" {
		t.Errorf("快照结果应记入清单: %+v", m.Kopia)
	}
}

func TestRunKopiaDisabledIsNoop(t *testing.T) {
	rc := newRunCollector("2026-09-21", time.Now())
	if err := runKopia(context.Background(), &config{backupDir: t.TempDir()}, t.TempDir(), "2026-09-21", rc); err != nil {
		t.Errorf("未启用 Kopia 时应直接返回: %v", err)
	}
	if m := rc.finish(time.Second, "Local"); m.Kopia != nil {
		t.Errorf("未启用时不应写异地信息: %+v", m.Kopia)
	}
}
