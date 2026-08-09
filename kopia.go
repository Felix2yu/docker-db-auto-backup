package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type kopiaClient struct {
	cfg *kopiaConfig
}

func newKopiaClient(cfg *config) *kopiaClient {
	if cfg == nil || cfg.kopia == nil {
		return nil
	}
	return &kopiaClient{cfg: cfg.kopia}
}

func (k *kopiaClient) globalArgs() []string {
	return []string{"--config-file", k.cfg.configFile}
}

func (k *kopiaClient) repositoryTypeArg() string {
	if k.cfg.repositoryType == "posix" {
		return "filesystem"
	}
	return k.cfg.repositoryType
}

func (k *kopiaClient) repositoryArgs(action string) []string {
	args := []string{"repository", action, k.repositoryTypeArg()}
	if k.cfg.repositoryFlags != "" {
		args = append(args, strings.Fields(k.cfg.repositoryFlags)...)
	}
	return args
}

func (k *kopiaClient) run(ctx context.Context, args ...string) (string, error) {
	full := append(k.globalArgs(), args...)
	cmd := exec.CommandContext(ctx, "kopia", full...)
	cmd.Env = append(os.Environ(), "KOPIA_PASSWORD="+k.cfg.password)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("kopia 命令失败: %w: %s", err, msg)
	}
	return stdout.String(), nil
}

func (k *kopiaClient) ensureRepository(ctx context.Context) error {
	if _, err := k.run(ctx, "repository", "status"); err == nil {
		return nil
	}
	action := "connect"
	if k.cfg.createRepository {
		action = "create"
	}
	if _, err := k.run(ctx, k.repositoryArgs(action)...); err != nil {
		return fmt.Errorf("连接 kopia 仓库失败: %w", err)
	}
	if _, err := k.run(ctx, "repository", "status"); err != nil {
		return fmt.Errorf("kopia 仓库校验失败: %w", err)
	}
	return nil
}

func (k *kopiaClient) snapshotCreate(ctx context.Context, path string) error {
	if _, err := k.run(ctx, "snapshot", "create", path); err != nil {
		return err
	}
	return nil
}
