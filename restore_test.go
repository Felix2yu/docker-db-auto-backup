package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
