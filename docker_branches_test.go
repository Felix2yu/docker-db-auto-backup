package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

// 镜像识别的兜底链：RepoTags → RepoDigests → Config.Image → 容器标签，
// 每一环都要能单独命中，否则换个镜像分发方式就"静默识别不到"。
func TestContainerImageNamesFallbackChain(t *testing.T) {
	ctx := context.Background()

	t.Run("RepoTags", func(t *testing.T) {
		fake := newFakeAPIClient()
		fake.inspect["c1"] = container.InspectResponse{Config: &container.Config{Image: "postgres:14"}}
		fake.imageTags["postgres:14"] = []string{"postgres:14", "postgres:latest"}
		dc := newFakeDockerClient(fake)

		names, err := dc.containerImageNames(ctx, "c1")
		if err != nil || len(names) == 0 || names[0] != "postgres" {
			t.Fatalf("names = %v, err = %v", names, err)
		}
		// 命中缓存后即便 API 报错也应返回同样的结果
		fake.inspectErr = errors.New("boom")
		again, err := dc.containerImageNames(ctx, "c1")
		if err != nil || again[0] != "postgres" {
			t.Errorf("结果应来自缓存: %v, %v", again, err)
		}
	})

	t.Run("RepoDigests", func(t *testing.T) {
		fake := newFakeAPIClient()
		fake.inspect["c1"] = container.InspectResponse{Config: &container.Config{Image: "postgres@sha256:aaa"}}
		fake.imageTags["postgres@sha256:aaa"] = []string{}
		fake.imageDigests = map[string][]string{"postgres@sha256:aaa": {"docker.io/library/postgres@sha256:aaa"}}
		dc := newFakeDockerClient(fake)

		names, err := dc.containerImageNames(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 1 || names[0] != "postgres" {
			t.Errorf("应按 RepoDigests 兜底, got %v", names)
		}
	})

	t.Run("ConfigImage", func(t *testing.T) {
		fake := newFakeAPIClient()
		fake.inspect["c1"] = container.InspectResponse{Config: &container.Config{Image: "bitnami/postgresql:16"}}
		fake.imageTags["bitnami/postgresql:16"] = []string{}
		fake.imageInspectErr = errors.New("image unavailable")
		dc := newFakeDockerClient(fake)

		names, err := dc.containerImageNames(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 1 || names[0] != "bitnami/postgresql" {
			t.Errorf("ImageInspect 失败时应回退到 Config.Image, got %v", names)
		}
	})

	t.Run("按 digest 运行时不猜镜像名", func(t *testing.T) {
		fake := newFakeAPIClient()
		fake.inspect["c1"] = container.InspectResponse{Config: &container.Config{Image: "sha256:deadbeef"}}
		fake.imageTags["sha256:deadbeef"] = []string{}
		dc := newFakeDockerClient(fake)

		names, err := dc.containerImageNames(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 0 {
			t.Errorf("sha256 引用不应被当作镜像名, got %v", names)
		}
	})

	for _, key := range []string{"com.docker.compose.image", "org.opencontainers.image.ref.name"} {
		t.Run("标签兜底 "+key, func(t *testing.T) {
			fake := newFakeAPIClient()
			fake.inspect["c1"] = container.InspectResponse{
				Config: &container.Config{Labels: map[string]string{key: "docker.io/library/postgres:16"}},
			}
			fake.imageTags[""] = []string{}
			dc := newFakeDockerClient(fake)

			names, err := dc.containerImageNames(ctx, "c1")
			if err != nil {
				t.Fatal(err)
			}
			if len(names) != 1 || names[0] != "postgres" {
				t.Errorf("应从 %s 识别出镜像名, got %v", key, names)
			}
		})
	}

	t.Run("全都识别不到", func(t *testing.T) {
		fake := newFakeAPIClient()
		fake.inspect["c1"] = container.InspectResponse{Config: &container.Config{}}
		fake.imageTags[""] = []string{}
		dc := newFakeDockerClient(fake)

		names, err := dc.containerImageNames(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 0 {
			t.Errorf("got %v", names)
		}
	})

	t.Run("inspect 失败", func(t *testing.T) {
		fake := newFakeAPIClient()
		fake.inspectErr = errors.New("no such container")
		dc := newFakeDockerClient(fake)
		if _, err := dc.containerImageNames(ctx, "c1"); err == nil {
			t.Error("inspect 失败应返回错误")
		}
	})

	t.Run("无 Config", func(t *testing.T) {
		fake := newFakeAPIClient()
		fake.inspect["c1"] = container.InspectResponse{}
		dc := newFakeDockerClient(fake)
		if names, err := dc.containerImageNames(ctx, "c1"); err != nil || len(names) != 0 {
			t.Errorf("got %v, %v", names, err)
		}
	})
}

func TestContainerBackupProviderLabel(t *testing.T) {
	ctx := context.Background()

	fake := newFakeAPIClient()
	fake.inspect["c1"] = container.InspectResponse{
		Config: &container.Config{Labels: map[string]string{labelBackupProvider: " Postgres "}},
	}
	if got := newFakeDockerClient(fake).containerBackupProviderLabel(ctx, "c1"); got != "postgres" {
		t.Errorf("标签应被规范化, got %q", got)
	}

	fake = newFakeAPIClient()
	fake.inspect["c1"] = container.InspectResponse{}
	if got := newFakeDockerClient(fake).containerBackupProviderLabel(ctx, "c1"); got != "" {
		t.Errorf("无 Config 时应返回空, got %q", got)
	}

	fake = newFakeAPIClient()
	fake.inspectErr = errors.New("boom")
	if got := newFakeDockerClient(fake).containerBackupProviderLabel(ctx, "c1"); got != "" {
		t.Errorf("inspect 失败时应返回空, got %q", got)
	}
}

func TestContainerImageRefErrors(t *testing.T) {
	ctx := context.Background()

	fake := newFakeAPIClient()
	fake.inspect["c1"] = container.InspectResponse{Image: "sha256:abc"}
	dc := newFakeDockerClient(fake)
	if ref, err := dc.containerImageRef(ctx, "c1"); err != nil || ref != "sha256:abc" {
		t.Errorf("got %q, %v", ref, err)
	}

	fake = newFakeAPIClient()
	fake.inspect["c1"] = container.InspectResponse{}
	if _, err := newFakeDockerClient(fake).containerImageRef(ctx, "c1"); err == nil {
		t.Error("既无镜像 ID 也无 Config.Image 时应报错")
	}

	fake = newFakeAPIClient()
	fake.inspect["c1"] = container.InspectResponse{Config: &container.Config{}}
	if _, err := newFakeDockerClient(fake).containerImageRef(ctx, "c1"); err == nil {
		t.Error("Config 里也没有镜像时应报错")
	}

	fake = newFakeAPIClient()
	fake.inspectErr = errors.New("boom")
	if _, err := newFakeDockerClient(fake).containerImageRef(ctx, "c1"); err == nil {
		t.Error("inspect 失败应传递错误")
	}
}

func TestContainerEnvParsingAndCache(t *testing.T) {
	ctx := context.Background()
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		return []byte("A=1\n\nB=2=3\nNOEQUALS\n"), nil, 0
	}
	dc := newFakeDockerClient(fake)

	env, err := dc.containerEnv(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if env["A"] != "1" || env["B"] != "2=3" {
		t.Errorf("应解析带 = 的值并忽略空行/无等号行: %v", env)
	}
	if _, ok := env["NOEQUALS"]; ok {
		t.Error("无等号的行不应进入环境")
	}

	fake.execCreateErr = errors.New("boom")
	if again, err := dc.containerEnv(ctx, "c1"); err != nil || again["A"] != "1" {
		t.Errorf("第二次应命中缓存: %v, %v", again, err)
	}

	dc2 := newFakeDockerClient(&fakeAPIClient{
		inspect: map[string]container.InspectResponse{}, imageTags: map[string][]string{},
		execs: map[string]*execResult{}, execCreateErr: errors.New("无 exec"),
	})
	if _, err := dc2.containerEnv(ctx, "c9"); err == nil {
		t.Error("exec 失败应返回错误")
	}
}

func TestDockerAPIErrorPaths(t *testing.T) {
	ctx := context.Background()

	fake := newFakeAPIClient()
	fake.execCreateErr = errors.New("container not running")
	dc := newFakeDockerClient(fake)
	if _, _, err := dc.startExec(ctx, "c1", []string{"env"}, nil); err == nil {
		t.Error("ExecCreate 失败应返回错误")
	}
	if _, err := dc.execCollect(ctx, "c1", []string{"env"}, nil); err == nil {
		t.Error("execCollect 应传递 ExecCreate 错误")
	}
	if ok, err := dc.hasBinary(ctx, "c1", "mysqldump"); err == nil || ok {
		t.Errorf("hasBinary 应报错, got %v %v", ok, err)
	}
	if err := writeBackup(ctx, &config{backupDir: t.TempDir(), compression: "plain"}, dc, "c1",
		[]string{"pg_dumpall"}, nil, "/tmp/x.sql", "sql", "pg", false); err == nil {
		t.Error("writeBackup 应报告创建 exec 失败")
	}

	fake = newFakeAPIClient()
	fake.execAttachErr = errors.New("attach refused")
	dc = newFakeDockerClient(fake)
	if _, _, err := dc.startExec(ctx, "c1", []string{"env"}, nil); err == nil {
		t.Error("ExecAttach 失败应返回错误")
	}
	if ok, err := dc.hasBinary(ctx, "c1", "mysqldump"); err == nil || ok {
		t.Errorf("hasBinary 应报错, got %v %v", ok, err)
	}

	fake = newFakeAPIClient()
	fake.execInspectErr = errors.New("inspect failed")
	dc = newFakeDockerClient(fake)
	if _, err := dc.execExitCode(ctx, "exec-1"); err == nil {
		t.Error("ExecInspect 失败应返回错误")
	}
	if _, err := dc.execCollect(ctx, "c1", []string{"env"}, nil); err == nil {
		t.Error("execCollect 应传递 ExecInspect 错误")
	}
	if _, err := dc.hasBinary(ctx, "c1", "mysqldump"); err == nil {
		t.Error("hasBinary 应传递 ExecInspect 错误")
	}
}

func TestHasBinaryPerBinaryCache(t *testing.T) {
	ctx := context.Background()
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if strings.Join(cmd, " ") == "which mariadb-dump" {
			return nil, nil, 1
		}
		return []byte("/usr/bin/mysqldump\n"), nil, 0
	}
	dc := newFakeDockerClient(fake)

	if ok, err := dc.hasBinary(ctx, "c1", "mysqldump"); err != nil || !ok {
		t.Errorf("mysqldump 应存在: %v %v", ok, err)
	}
	if ok, err := dc.hasBinary(ctx, "c1", "mariadb-dump"); err != nil || ok {
		t.Errorf("mariadb-dump 应不存在: %v %v", ok, err)
	}

	// 同一容器的第二个二进制名要落到缓存的外层 map 里再查询
	fake.execCreateErr = errors.New("不再允许 exec")
	if ok, err := dc.hasBinary(ctx, "c1", "mysqldump"); err != nil || !ok {
		t.Errorf("应命中缓存: %v %v", ok, err)
	}
	if _, err := dc.hasBinary(ctx, "c1", "pg_dump"); err == nil {
		t.Error("未缓存过的二进制应触发真实查询并报错")
	}
}

func TestNewDockerClient(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	dc, err := newDockerClient(context.Background())
	if err != nil {
		t.Fatalf("新建客户端不应连接: %v", err)
	}
	if dc == nil || dc.api == nil || dc.envCache == nil || dc.binCache == nil || dc.nameCache == nil {
		t.Fatalf("客户端未正确初始化: %+v", dc)
	}

	t.Setenv("DOCKER_HOST", "无法解析的 host")
	if _, err := newDockerClient(context.Background()); err == nil {
		t.Error("非法 DOCKER_HOST 应返回错误")
	}
}

func TestListContainersErrorPropagates(t *testing.T) {
	fake := newFakeAPIClient()
	fake.listErr = errors.New("daemon down")
	if _, err := newFakeDockerClient(fake).listContainers(context.Background()); err == nil {
		t.Error("应传递 ContainerList 错误")
	}
}
