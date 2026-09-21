package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// restore drill（B7）：把本次备份导入一个临时容器，验证"可恢复"而不只是"格式合法"。
//
// 演练失败只作为警告上报，不改变备份本身的成败判定。
const (
	drillTimeout      = 10 * time.Minute
	drillReadyTimeout = 90 * time.Second
	drillRemotePath   = "/tmp/auto-backup-restore"
)

func runRestoreDrill(ctx context.Context, cfg *config, dc *dockerClient, backupBase string, m *backupManifest, plans []*containerPlan) []string {
	if len(plans) == 0 {
		return nil
	}
	drillCtx, cancel := context.WithTimeout(ctx, drillTimeout)
	defer cancel()

	var warnings []string
	for _, plan := range plans {
		if plan.provider == nil {
			continue
		}
		if plan.provider.name != "postgres" && plan.provider.name != "mysql" {
			logDebug("恢复演练暂不支持该类型，已跳过", "provider", plan.provider.name)
			continue
		}
		var target containerManifest
		found := false
		for _, c := range m.Containers {
			if c.Name == plan.name {
				target = c
				found = true
				break
			}
		}
		if !found || len(target.Files) == 0 {
			continue
		}

		file := pickDrillFile(cfg, target)
		if file == "" {
			continue
		}
		logInfo("开始恢复演练", "container", plan.name, "file", file)
		if err := drillRestore(drillCtx, cfg, dc, plan, file); err != nil {
			warnings = append(warnings, fmt.Sprintf("容器 %s 恢复演练失败：%v", plan.name, err))
			continue
		}
		logInfo("恢复演练通过", "container", plan.name)
	}
	return warnings
}

// pickDrillFile 选择用于演练的文件：优先用户库，其次系统库/全库文件。
func pickDrillFile(cfg *config, c containerManifest) string {
	var fallback string
	for _, f := range c.Files {
		full := filepath.Join(cfg.backupDir, f.Path)
		if fallback == "" {
			fallback = full
		}
		if !f.System {
			return full
		}
	}
	return fallback
}

func drillRestore(ctx context.Context, cfg *config, dc *dockerClient, plan *containerPlan, dumpFile string) error {
	names, err := dc.containerImageNames(ctx, plan.c.ID)
	if err != nil || len(names) == 0 {
		return fmt.Errorf("无法确定基础镜像: %v", err)
	}
	image := names[0]

	env, envErr := dc.containerEnv(ctx, plan.c.ID)
	if envErr != nil {
		env = map[string]string{}
	}
	drillEnv := drillEnvironment(plan.provider.name, env, cfg)

	name := fmt.Sprintf("auto-backup-drill-%d", os.Getpid())
	created, err := dc.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: image, Env: drillEnv},
		Name:   name,
	})
	if err != nil {
		return fmt.Errorf("创建演练容器失败: %w", err)
	}
	containerID := created.ID
	defer func() {
		if _, err := dc.api.ContainerRemove(context.Background(), containerID, client.ContainerRemoveOptions{Force: true}); err != nil {
			logWarn("清理演练容器失败", "container", containerID, "error", err)
		}
	}()

	if _, err := dc.api.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("启动演练容器失败: %w", err)
	}

	readyCmd := drillReadyCommand(plan.provider.name, cfg, env)
	if err := waitForReady(ctx, dc, containerID, readyCmd, drillEnv); err != nil {
		return err
	}

	if err := copyFileIntoContainer(ctx, dc, containerID, dumpFile, "dump.sql"); err != nil {
		return fmt.Errorf("复制备份文件失败: %w", err)
	}

	importCmd := drillImportCommand(plan.provider.name, cfg, env)
	if _, err := dc.execCollect(ctx, containerID, importCmd, drillEnv); err != nil {
		return fmt.Errorf("导入备份失败: %w", err)
	}

	verifyCmd := drillVerifyCommand(plan.provider.name, cfg, env)
	out, err := dc.execCollect(ctx, containerID, verifyCmd, drillEnv)
	if err != nil {
		return fmt.Errorf("恢复后校验失败: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("恢复后校验未返回任何结果")
	}
	return nil
}

// drillEnvironment 构造演练容器所需的环境变量：只保留连接相关，避免带入生产配置。
func drillEnvironment(provider string, env map[string]string, cfg *config) []string {
	var out []string
	keep := []string{
		"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB",
		"MYSQL_ROOT_PASSWORD", "MARIADB_ROOT_PASSWORD",
		"MYSQL_USER", "MYSQL_PASSWORD", "MYSQL_DATABASE",
	}
	for _, k := range keep {
		if v := env[k]; v != "" {
			out = append(out, k+"="+v)
		}
	}
	if provider == "postgres" {
		if env["POSTGRES_PASSWORD"] == "" {
			// 允许无密码本地连接，避免官方镜像因缺少密码而拒绝启动。
			out = append(out, "POSTGRES_HOST_AUTH_METHOD=trust")
		}
		out = append(out, "POSTGRES_INITDB_ARGS=--no-sync")
	}
	if cfg != nil && cfg.mysqlPassword != "" && provider == "mysql" {
		out = append(out, "MYSQL_PWD="+cfg.mysqlPassword)
	}
	return out
}

func drillReadyCommand(provider string, cfg *config, env map[string]string) []string {
	if provider == "mysql" {
		return []string{"bash", "-c", "mysqladmin ping -u root --silent"}
	}
	user := postgresUser(cfg, env)
	return []string{"pg_isready", "-U", user}
}

func drillImportCommand(provider string, cfg *config, env map[string]string) []string {
	path := drillRemotePath + "/dump.sql"
	if provider == "mysql" {
		user := "root"
		if cfg != nil && cfg.mysqlUser != "" {
			user = cfg.mysqlUser
		} else if u := env["MYSQL_USER"]; u != "" {
			user = u
		}
		return []string{"bash", "-c", fmt.Sprintf("mysql -u %s < %s", shellQuote(user), shellQuote(path))}
	}
	user := postgresUser(cfg, env)
	return []string{"bash", "-c", fmt.Sprintf("psql -U %s -f %s postgres", shellQuote(user), shellQuote(path))}
}

func drillVerifyCommand(provider string, cfg *config, env map[string]string) []string {
	if provider == "mysql" {
		user := "root"
		if cfg != nil && cfg.mysqlUser != "" {
			user = cfg.mysqlUser
		} else if u := env["MYSQL_USER"]; u != "" {
			user = u
		}
		return []string{"bash", "-c", fmt.Sprintf("mysql -u %s -s --skip-column-names -e \"SELECT COUNT(*) FROM information_schema.tables\"", shellQuote(user))}
	}
	user := postgresUser(cfg, env)
	return []string{"psql", "-U", user, "-t", "-A", "-c", "SELECT count(*) FROM information_schema.tables"}
}

func waitForReady(ctx context.Context, dc *dockerClient, containerID string, cmd, env []string) error {
	deadline := time.Now().Add(drillReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := dc.execCollect(ctx, containerID, cmd, env); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("演练容器在 %s 内未就绪: %v", drillReadyTimeout, lastErr)
}

// copyFileIntoContainer 把本地文件打包成 tar 后写入容器。
func copyFileIntoContainer(ctx context.Context, dc *dockerClient, containerID, localPath, remoteName string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: remoteName,
		Mode: 0o644,
		Size: int64(len(data)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	_, err = dc.api.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{
		DestinationPath: drillRemotePath,
		Content:         &buf,
	})
	return err
}
