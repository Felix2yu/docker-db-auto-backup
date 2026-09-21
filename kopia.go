package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

// policyArgs 合并压缩策略与保留策略（B6）。
func (k *kopiaClient) policyArgs() []string {
	args := []string{"policy", "set", "--global"}
	if k.cfg.policyCompression != "" {
		args = append(args, "--compression", k.cfg.policyCompression)
	}
	if k.cfg.retentionFlags != "" {
		args = append(args, strings.Fields(k.cfg.retentionFlags)...)
	}
	return args
}

func (k *kopiaClient) ensurePolicy(ctx context.Context) error {
	if k.cfg.policyCompression == "" && k.cfg.retentionFlags == "" {
		return nil
	}
	if _, err := k.run(ctx, k.policyArgs()...); err != nil {
		return fmt.Errorf("设置 kopia 策略失败: %w", err)
	}
	return nil
}

// snapshotCreate 推送快照并解析快照 ID，便于在通知与清单中追溯（B6）。
func (k *kopiaClient) snapshotCreate(ctx context.Context, path, description string) (string, error) {
	args := []string{"snapshot", "create", "--json"}
	if description != "" {
		args = append(args, "--description", description)
	}
	args = append(args, path)
	out, err := k.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return parseSnapshotID(out), nil
}

func parseSnapshotID(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	// 优先取最后一行 JSON（kopia 可能先输出进度信息）。
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var parsed struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &parsed); err == nil && parsed.ID != "" {
			return parsed.ID
		}
	}
	return ""
}

// maybeMaintenance 按间隔执行仓库维护（B6），避免远端仓库只增不减、性能退化。
func (k *kopiaClient) maybeMaintenance(ctx context.Context, backupDir string) error {
	if k.cfg.maintenanceInterval <= 0 {
		return nil
	}
	marker := filepath.Join(filepath.Dir(k.cfg.configFile), ".last-maintenance")
	shouldRun := true
	if data, err := os.ReadFile(marker); err == nil {
		if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data))); err == nil {
			shouldRun = time.Since(ts) >= k.cfg.maintenanceInterval
		}
	}
	if !shouldRun {
		return nil
	}
	logInfo("开始执行 kopia 仓库维护")
	if _, err := k.run(ctx, "maintenance", "run", "--full"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err == nil {
		os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)), 0o644)
	}
	return nil
}

// runKopia 串联仓库校验、策略、快照与维护，并把结果写进清单。
func runKopia(ctx context.Context, cfg *config, path, dateDir string, collector *runCollector) error {
	k := newKopiaClient(cfg)
	if k == nil {
		return nil
	}
	if err := k.ensureRepository(ctx); err != nil {
		collector.setKopia(&kopiaManifestInfo{Pushed: false, Error: err.Error()})
		return err
	}
	if err := k.ensurePolicy(ctx); err != nil {
		collector.setKopia(&kopiaManifestInfo{Pushed: false, Error: err.Error()})
		return err
	}

	description := fmt.Sprintf("auto-backup %s", dateDir)
	id, err := k.snapshotCreate(ctx, path, description)
	if err != nil {
		collector.setKopia(&kopiaManifestInfo{Pushed: false, Error: err.Error()})
		return err
	}
	collector.setKopia(&kopiaManifestInfo{Pushed: true, SnapshotID: id})
	logInfo("Kopia 快照已创建", "snapshot", id, "description", description)

	// 维护失败只告警，不影响本次备份结果。
	if err := k.maybeMaintenance(ctx, cfg.backupDir); err != nil {
		logWarn("kopia 仓库维护失败", "error", err)
	}
	return nil
}
