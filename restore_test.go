package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

func TestDrillRestoreUsesRunningImageAndRemovesVolumes(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "appdb.sql")
	if err := os.WriteFile(dump, []byte("-- PostgreSQL database dump\n-- PostgreSQL database dump complete\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const imageID = "sha256:1f2d3c4b5a6978879685a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3"
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	fake.inspect["c1"] = container.InspectResponse{
		Image:  imageID,
		Config: &container.Config{Image: "postgres"},
	}
	dc := newFakeDockerClient(fake)

	plan := &containerPlan{
		c:        container.Summary{ID: "c1"},
		name:     "pg",
		provider: providerByName("postgres"),
	}
	if err := drillRestore(context.Background(), &config{backupDir: dir}, dc, plan, dump); err != nil {
		t.Fatalf("drillRestore: %v", err)
	}

	if len(fake.created) != 1 {
		t.Fatalf("应创建 1 个演练容器, got %d", len(fake.created))
	}
	// 仓库名不带 tag，用它建容器会退化成 :latest，演练验的就不是源库那个版本。
	if got := fake.created[0].Config.Image; got != imageID {
		t.Errorf("演练容器应使用源容器实际镜像 ID, got %q", got)
	}
	if len(fake.removeOpts) != 1 {
		t.Fatalf("演练结束应销毁容器, got %d", len(fake.removeOpts))
	}
	if !fake.removeOpts[0].RemoveVolumes {
		t.Error("必须连数据卷一起移除，否则每次演练泄漏一个匿名卷")
	}
	if !fake.removeOpts[0].Force {
		t.Error("应以 force 移除演练容器")
	}
}

func TestContainerImageRefFallsBackToConfigImage(t *testing.T) {
	fake := newFakeAPIClient()
	fake.inspect["c1"] = container.InspectResponse{Config: &container.Config{Image: "mysql:8.4"}}
	dc := newFakeDockerClient(fake)

	ref, err := dc.containerImageRef(context.Background(), "c1")
	if err != nil {
		t.Fatalf("containerImageRef: %v", err)
	}
	if ref != "mysql:8.4" {
		t.Errorf("无镜像 ID 时应回退到 Config.Image, got %q", ref)
	}
}

// drillExecControl 控制演练链路上各个 exec 调用的返回，便于逐条覆盖失败分支。
type drillExecControl struct {
	readyFails  int  // 前 N 次就绪探测失败
	readyAlways bool // 就绪始终失败
	envErr      bool
	importErr   bool
	verifyEmpty bool
}

func (c *drillExecControl) handler(cmd []string) ([]byte, []byte, int) {
	joined := strings.Join(cmd, " ")
	switch {
	case cmd[0] == "env":
		if c.envErr {
			return nil, []byte("env: 不可用"), 1
		}
		return []byte("POSTGRES_USER=postgres\nPOSTGRES_PASSWORD=secret\nMYSQL_ROOT_PASSWORD=rootpass\n"), nil, 0
	case strings.Contains(joined, "pg_isready") || strings.Contains(joined, "mysqladmin ping"):
		if c.readyAlways || c.readyFails > 0 {
			c.readyFails--
			return nil, []byte("not ready yet"), 1
		}
		return []byte("accepting connections\n"), nil, 0
	case strings.Contains(joined, "-f ") || strings.Contains(joined, "< "):
		if c.importErr {
			return nil, []byte("ERROR: 语法错误"), 1
		}
		return nil, nil, 0
	case strings.Contains(joined, "information_schema.tables"):
		if c.verifyEmpty {
			return []byte("\n"), nil, 0
		}
		return []byte("42\n"), nil, 0
	}
	return []byte("ok\n"), nil, 0
}

const drillImageID = "sha256:1f2d3c4b5a6978879685a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3"

// drillFixture 造一套"源容器 + 本次备份产物"的演练环境。
func drillFixture(t *testing.T, providerName string, ctl *drillExecControl) (*config, *dockerClient, *fakeAPIClient, *containerPlan, *backupManifest, string) {
	t.Helper()
	p := providerByName(providerName)
	if p == nil {
		t.Fatalf("未知 provider %q", providerName)
	}
	cfg := &config{backupDir: t.TempDir(), checksumEnabled: true}
	fake := newFakeAPIClient()
	if ctl == nil {
		ctl = &drillExecControl{}
	}
	fake.execHandler = ctl.handler
	fake.inspect["c1"] = container.InspectResponse{
		Image:  drillImageID,
		Config: &container.Config{Image: "postgres:16"},
	}
	dc := newFakeDockerClient(fake)

	base := filepath.Join(cfg.backupDir, "2026-09-21")
	if err := os.MkdirAll(filepath.Join(base, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(base, "db", "appdb.sql")
	if err := os.WriteFile(dump, []byte(validSQLDump), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := &containerPlan{
		c:        container.Summary{ID: "c1"},
		name:     "db",
		provider: p,
		mode:     modeSingle,
	}
	m := &backupManifest{Version: manifestVersion, Date: "2026-09-21", Status: statusSuccess,
		Containers: []containerManifest{{Name: "db", Provider: providerName, Mode: modeSingle, Files: []fileEntry{
			{Path: "2026-09-21/db/appdb.sql", Size: int64(len(validSQLDump)), Database: "appdb"},
		}}}}
	return cfg, dc, fake, plan, m, base
}

func TestRunRestoreDrillSkipsUnsupportedPlans(t *testing.T) {
	cfg, dc, fake, plan, m, _ := drillFixture(t, "redis", nil)

	redisPlan := plan
	redisPlan.provider = providerByName("redis")
	nilProvider := &containerPlan{c: container.Summary{ID: "c1"}, name: "db"}
	unrelated := &containerPlan{c: container.Summary{ID: "c1"}, name: "gone", provider: providerByName("postgres")}
	noFiles := &containerPlan{c: container.Summary{ID: "c1"}, name: "empty", provider: providerByName("postgres")}
	m.Containers = []containerManifest{{Name: "empty", Provider: "postgres", Files: []fileEntry{}}}

	if got := runRestoreDrill(context.Background(), cfg, dc, "", m, nil); got != nil {
		t.Errorf("没有计划时应直接返回 nil, got %v", got)
	}
	if got := runRestoreDrill(context.Background(), cfg, dc, "", m,
		[]*containerPlan{redisPlan, nilProvider, unrelated, noFiles}); len(got) != 0 {
		t.Errorf("不支持/无产出的计划不应产生警告, got %v", got)
	}
	if len(fake.created) != 0 {
		t.Errorf("不应创建演练容器, got %d", len(fake.created))
	}
}

func TestRunRestoreDrillSuccess(t *testing.T) {
	for _, providerName := range []string{"postgres", "mysql"} {
		t.Run(providerName, func(t *testing.T) {
			cfg, dc, fake, plan, m, _ := drillFixture(t, providerName, nil)
			warnings := runRestoreDrill(context.Background(), cfg, dc, "", m, []*containerPlan{plan})
			if len(warnings) != 0 {
				t.Fatalf("演练通过时不应有警告, got %v", warnings)
			}
			if len(fake.created) != 1 {
				t.Fatalf("应创建 1 个演练容器, got %d", len(fake.created))
			}
			if got := fake.created[0].Config.Image; got != drillImageID {
				t.Errorf("演练应使用源容器镜像 ID, got %q", got)
			}
			if len(fake.started) != 1 || fake.started[0] != "drill-1" {
				t.Errorf("应启动演练容器, got %v", fake.started)
			}
			if len(fake.removeOpts) != 1 || !fake.removeOpts[0].RemoveVolumes {
				t.Errorf("应连卷销毁, got %v", fake.removeOpts)
			}
			// 演练文件确实被送进了容器
			if len(fake.copyDest) != 1 || fake.copyDest[0] != drillRemotePath {
				t.Errorf("copy 目标目录 = %v, want %s", fake.copyDest, drillRemotePath)
			}
			if !bytes.Contains(fake.copyContent, []byte("Dump completed")) {
				t.Error("演练容器应收到 dump 内容")
			}
		})
	}
}

func TestRunRestoreDrillReportsFailureAsWarning(t *testing.T) {
	cfg, dc, fake, plan, m, _ := drillFixture(t, "postgres", nil)
	fake.createErr = errors.New("no such image")

	warnings := runRestoreDrill(context.Background(), cfg, dc, "", m, []*containerPlan{plan})
	if len(warnings) != 1 {
		t.Fatalf("应上报 1 条警告, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "db") || !strings.Contains(warnings[0], "恢复演练失败") {
		t.Errorf("警告应包含容器名与原因, got %q", warnings[0])
	}
	// 创建失败时不应残留销毁调用
	if len(fake.removeOpts) != 0 {
		t.Errorf("容器没建起来就不应移除, got %v", fake.removeOpts)
	}
}

func TestDrillRestoreErrorPaths(t *testing.T) {
	t.Run("无法确定镜像", func(t *testing.T) {
		cfg, dc, fake, plan, _, _ := drillFixture(t, "postgres", nil)
		delete(fake.inspect, "c1")
		if err := drillRestore(context.Background(), cfg, dc, plan, filepath.Join(cfg.backupDir, "x.sql")); err == nil {
			t.Error("inspect 失败应返回错误")
		}
	})

	t.Run("启动失败", func(t *testing.T) {
		cfg, dc, fake, plan, _, _ := drillFixture(t, "postgres", nil)
		fake.startErr = errors.New("boom")
		err := drillRestore(context.Background(), cfg, dc, plan, dumpOfFile(t, cfg))
		if err == nil || !strings.Contains(err.Error(), "启动演练容器失败") {
			t.Errorf("got %v", err)
		}
		if len(fake.removeOpts) != 1 {
			t.Error("启动失败也要销毁容器")
		}
	})

	t.Run("未就绪", func(t *testing.T) {
		cfg, dc, _, plan, _, _ := drillFixture(t, "postgres", &drillExecControl{readyAlways: true})
		defer withReadyTimeout(20 * time.Millisecond)()
		err := drillRestore(context.Background(), cfg, dc, plan, dumpOfFile(t, cfg))
		if err == nil || !strings.Contains(err.Error(), "未就绪") {
			t.Errorf("got %v", err)
		}
	})

	t.Run("上下文取消", func(t *testing.T) {
		cfg, dc, _, plan, _, _ := drillFixture(t, "postgres", &drillExecControl{readyAlways: true})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := drillRestore(ctx, cfg, dc, plan, dumpOfFile(t, cfg)); err == nil {
			t.Error("ctx 取消时应返回错误")
		}
	})

	t.Run("复制失败", func(t *testing.T) {
		cfg, dc, fake, plan, _, _ := drillFixture(t, "postgres", nil)
		fake.copyErr = errors.New("archive is full")
		err := drillRestore(context.Background(), cfg, dc, plan, dumpOfFile(t, cfg))
		if err == nil || !strings.Contains(err.Error(), "复制备份文件失败") {
			t.Errorf("got %v", err)
		}
	})

	t.Run("导入失败", func(t *testing.T) {
		cfg, dc, _, plan, _, _ := drillFixture(t, "postgres", &drillExecControl{importErr: true})
		err := drillRestore(context.Background(), cfg, dc, plan, dumpOfFile(t, cfg))
		if err == nil || !strings.Contains(err.Error(), "导入备份失败") {
			t.Errorf("got %v", err)
		}
	})

	t.Run("校验无结果", func(t *testing.T) {
		cfg, dc, _, plan, _, _ := drillFixture(t, "postgres", &drillExecControl{verifyEmpty: true})
		err := drillRestore(context.Background(), cfg, dc, plan, dumpOfFile(t, cfg))
		if err == nil || !strings.Contains(err.Error(), "未返回任何结果") {
			t.Errorf("got %v", err)
		}
	})

	t.Run("源容器 env 读取失败时仍可演练", func(t *testing.T) {
		cfg, dc, _, plan, _, _ := drillFixture(t, "postgres", &drillExecControl{envErr: true})
		if err := drillRestore(context.Background(), cfg, dc, plan, dumpOfFile(t, cfg)); err != nil {
			t.Errorf("env 失败应退化为空环境继续: %v", err)
		}
	})
}

func dumpOfFile(t *testing.T, cfg *config) string {
	t.Helper()
	dir := filepath.Join(cfg.backupDir, "2026-09-21", "db")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "appdb.sql")
	if err := os.WriteFile(p, []byte(validSQLDump), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// withReadyTimeout 临时收紧就绪等待参数，返回还原函数。
func withReadyTimeout(d time.Duration) func() {
	oldTimeout, oldInterval := drillReadyTimeout, drillReadyInterval
	drillReadyTimeout, drillReadyInterval = d, time.Millisecond
	return func() { drillReadyTimeout, drillReadyInterval = oldTimeout, oldInterval }
}

func TestPickDrillFile(t *testing.T) {
	cfg := &config{backupDir: "/var/backups"}

	if got := pickDrillFile(cfg, containerManifest{}); got != "" {
		t.Errorf("无文件时应返回空, got %q", got)
	}

	// 优先用户库，而不是排在最前面的系统库
	mixed := containerManifest{Files: []fileEntry{
		{Path: "2026-09-21/db/system/globals.sql", System: true},
		{Path: "2026-09-21/db/appdb.sql"},
	}}
	if got := pickDrillFile(cfg, mixed); got != "/var/backups/2026-09-21/db/appdb.sql" {
		t.Errorf("应选用户库, got %q", got)
	}

	// 全是系统库时退回第一个
	onlySystem := containerManifest{Files: []fileEntry{
		{Path: "2026-09-21/db/system/globals.sql", System: true},
		{Path: "2026-09-21/db/system/mysql.sql", System: true},
	}}
	if got := pickDrillFile(cfg, onlySystem); got != "/var/backups/2026-09-21/db/system/globals.sql" {
		t.Errorf("无用户库时应取第一个, got %q", got)
	}
}

func TestDrillCommandsAndEnvironment(t *testing.T) {
	env := map[string]string{"POSTGRES_USER": "app", "MYSQL_USER": "backup"}

	if cmd := drillReadyCommand("mysql", nil, env); !strings.Contains(strings.Join(cmd, " "), "mysqladmin ping") {
		t.Errorf("mysql 就绪探测应使用 mysqladmin: %v", cmd)
	}
	if cmd := drillReadyCommand("postgres", nil, env); cmd[0] != "pg_isready" || cmd[2] != "app" {
		t.Errorf("pg 就绪探测应使用 POSTGRES_USER: %v", cmd)
	}

	// 导入与校验命令的用户优先级：显式配置 > 容器 env > 默认
	pgCfg := &config{pgUser: "pgbackup"}
	if cmd := drillImportCommand("postgres", pgCfg, env); !strings.Contains(strings.Join(cmd, " "), "-U pgbackup") {
		t.Errorf("应使用配置的备份账号: %v", cmd)
	}
	mysqlCfg := &config{mysqlUser: "mybackup", mysqlPassword: "pw"}
	if cmd := drillImportCommand("mysql", mysqlCfg, env); !strings.Contains(strings.Join(cmd, " "), "-u mybackup") {
		t.Errorf("mysql 导入命令应使用配置的账号: %v", cmd)
	}
	if cmd := drillVerifyCommand("mysql", nil, env); !strings.Contains(strings.Join(cmd, " "), "-u backup") {
		t.Errorf("未显式配置时应回退到 MYSQL_USER: %v", cmd)
	}
	if cmd := drillVerifyCommand("mysql", nil, map[string]string{}); !strings.Contains(strings.Join(cmd, " "), "-u root") {
		t.Errorf("都没有时应回退到 root: %v", cmd)
	}
	if cmd := drillVerifyCommand("postgres", nil, env); cmd[2] != "app" {
		t.Errorf("pg 校验应使用 POSTGRES_USER: %v", cmd)
	}

	// 演练环境只带连接相关变量；PG 无密码时用 trust 启动
	out := drillEnvironment("postgres", map[string]string{"POSTGRES_USER": "app", "TZ": "UTC"}, nil)
	joined := strings.Join(out, ",")
	if !strings.Contains(joined, "POSTGRES_USER=app") || strings.Contains(joined, "TZ") {
		t.Errorf("应只保留连接变量: %v", out)
	}
	if !strings.Contains(joined, "POSTGRES_HOST_AUTH_METHOD=trust") {
		t.Errorf("无密码时应允许 trust 本地连接: %v", out)
	}
	out = drillEnvironment("postgres", map[string]string{"POSTGRES_PASSWORD": "secret"}, nil)
	if strings.Contains(strings.Join(out, ","), "POSTGRES_HOST_AUTH_METHOD") {
		t.Errorf("有密码时不应放宽认证: %v", out)
	}
	out = drillEnvironment("mysql", map[string]string{"MYSQL_ROOT_PASSWORD": "root"}, mysqlCfg)
	if !strings.Contains(strings.Join(out, ","), "MYSQL_PWD=pw") {
		t.Errorf("mysql 演练应带上备份密码: %v", out)
	}
}

func TestWaitForReady(t *testing.T) {
	fake := newFakeAPIClient()
	ctl := &drillExecControl{readyFails: 2}
	fake.execHandler = ctl.handler
	dc := newFakeDockerClient(fake)
	defer withReadyTimeout(20 * time.Millisecond)()

	if err := waitForReady(context.Background(), dc, "c1", []string{"pg_isready"}, nil); err != nil {
		t.Errorf("重试后应就绪: %v", err)
	}
	if ctl.readyFails != 0 {
		t.Errorf("应消耗掉全部失败次数, got %d", ctl.readyFails)
	}

	ctl2 := &drillExecControl{readyAlways: true}
	fake.execHandler = ctl2.handler
	err := waitForReady(context.Background(), dc, "c1", []string{"pg_isready"}, nil)
	if err == nil || !strings.Contains(err.Error(), "未就绪") {
		t.Errorf("超时应报告未就绪, got %v", err)
	}
	if !strings.Contains(err.Error(), "not ready yet") {
		t.Errorf("错误里应带上最后一次探测的失败原因, got %v", err)
	}
}

func TestCopyFileIntoContainer(t *testing.T) {
	fake := newFakeAPIClient()
	dc := newFakeDockerClient(fake)
	dir := t.TempDir()
	path := filepath.Join(dir, "dump.sql")
	if err := os.WriteFile(path, []byte(validSQLDump), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyFileIntoContainer(context.Background(), dc, "c1", path, "dump.sql"); err != nil {
		t.Fatalf("copyFileIntoContainer: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(fake.copyContent))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar 解析失败: %v", err)
	}
	if hdr.Name != "dump.sql" || hdr.Size != int64(len(validSQLDump)) {
		t.Errorf("tar 头部不对: %+v", hdr)
	}
	body, _ := io.ReadAll(tr)
	if string(body) != validSQLDump {
		t.Errorf("内容不符: %q", body)
	}

	if err := copyFileIntoContainer(context.Background(), dc, "c1", filepath.Join(dir, "missing.sql"), "x"); err == nil {
		t.Error("本地文件缺失应返回错误")
	}
}

// 销毁演练容器失败只记日志，不能把本来成功的演练判成失败。
func TestDrillRestoreRemovalFailureIsNotFatal(t *testing.T) {
	cfg, dc, fake, plan, _, _ := drillFixture(t, "postgres", nil)
	fake.removeErr = errors.New("device or resource busy")

	if err := drillRestore(context.Background(), cfg, dc, plan, dumpOfFile(t, cfg)); err != nil {
		t.Errorf("销毁失败不应影响演练结果: %v", err)
	}
	if len(fake.removeOpts) != 1 {
		t.Errorf("应尝试销毁, got %d", len(fake.removeOpts))
	}
}
