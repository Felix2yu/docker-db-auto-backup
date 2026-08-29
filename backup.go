package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/schollz/progressbar/v3"
)

type backupResult struct {
	name         string
	providerType string
	dbs          []databaseInfo
}

type databaseInfo struct {
	name     string
	isSystem bool
}

func backup(ctx context.Context, cfg *config, dc *dockerClient, runAt time.Time) error {
	fmt.Println("开始备份...")

	containers, err := dc.listContainers(ctx)
	if err != nil {
		return err
	}

	dateDir := runAt.Format("2006-01-02")
	backupBase := filepath.Join(cfg.backupDir, dateDir)
	fmt.Printf("发现 %d 个容器，正在备份到 %s\n", len(containers), backupBase)
	if err := os.MkdirAll(backupBase, 0o755); err != nil {
		return err
	}
	applyOwnership(backupBase, cfg)

	hcURL := strings.TrimRight(cfg.healthchecksURL, "/")
	if hcURL != "" {
		hcPing(hcURL+"/start", "")
	}

	var (
		mu      sync.Mutex
		errs    []error
		results []backupResult
	)

	workers := cfg.workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(containers) {
		workers = len(containers)
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, c := range containers {
		wg.Add(1)
		sem <- struct{}{}
		go func(c container.Summary) {
			defer func() {
				<-sem
				wg.Done()
			}()
			res, err := backupContainer(ctx, cfg, dc, c, backupBase)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if res != nil {
				mu.Lock()
				results = append(results, *res)
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()

	if len(errs) > 0 {
		if hcURL != "" {
			hcPing(hcURL+"/fail", "")
		}
		return fmt.Errorf("%d 个容器备份失败: %w", len(errs), errs[0])
	}

	if cfg.kopiaEnabled() {
		kopia := newKopiaClient(cfg)
		if err := kopia.ensureRepository(ctx); err != nil {
			if hcURL != "" {
				hcPing(hcURL+"/fail", "")
			}
			return err
		}
		if err := kopia.ensurePolicy(ctx); err != nil {
			if hcURL != "" {
				hcPing(hcURL+"/fail", "")
			}
			return err
		}
		if err := kopia.snapshotCreate(ctx, backupBase); err != nil {
			if hcURL != "" {
				hcPing(hcURL+"/fail", "")
			}
			return err
		}
		fmt.Println("Kopia 快照已推送")
	}

	duration := time.Since(runAt)
	durationStr := fmt.Sprintf("%.2f 秒", duration.Seconds())
	if duration >= time.Minute {
		durationStr = fmt.Sprintf("%d 分钟 %d 秒",
			int(duration/time.Minute), int(duration%time.Minute/time.Second))
	}
	fmt.Printf("成功备份 %d 个容器，耗时 %s。\n", len(results), durationStr)

	tree := formatTree(results)

	if len(cfg.shoutrrrURLs) > 0 {
		notifyShoutrrr(ctx, cfg, cfg.shoutrrrURLs,
			fmt.Sprintf("成功备份 %d 个容器，耗时 %s。\n\n已备份容器:\n%s",
				len(results), durationStr, tree))
	}

	if cfg.retentionDays > 0 {
		cleanOldBackups(cfg, runAt)
	}

	if hcURL != "" {
		hcPing(hcURL, fmt.Sprintf("成功备份 %d 个容器，耗时 %s。\n\n已备份容器:\n%s",
			len(results), durationStr, tree))
	}
	return nil
}

func backupContainer(ctx context.Context, cfg *config, dc *dockerClient, c container.Summary, backupBase string) (*backupResult, error) {
	containerNames, err := dc.containerImageNames(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	provider := getBackupProvider(containerNames)
	if provider == nil {
		return nil, nil
	}

	name := containerName(c)
	var dbs []databaseInfo
	backedUp := false

	if cfg.singleDBMode && provider.singleDB != nil {
		entries, err := provider.singleDB(ctx, dc, c.ID)
		if err != nil {
			fmt.Printf("警告: 单数据库模式对 %s 失败: %v，将回退到默认模式\n", name, err)
		} else {
			for _, db := range entries {
				dbDir := filepath.Join(backupBase, name)
				if db.isSystem {
					dbDir = filepath.Join(dbDir, "system")
				}
				if err := os.MkdirAll(dbDir, 0o755); err != nil {
					return nil, err
				}
				applyOwnership(dbDir, cfg)

				backupFile := filepath.Join(dbDir,
					fmt.Sprintf("%s.%s%s", db.name, provider.fileExt,
						compressedExtension(cfg.effectiveCompression())))
				description := fmt.Sprintf("%s/%s (%s)", name, db.name, provider.name)
				if err := writeBackup(ctx, cfg, dc, c.ID, db.command, backupFile, provider.fileExt, description); err != nil {
					return nil, err
				}
				dbs = append(dbs, databaseInfo{name: db.name, isSystem: db.isSystem})
			}
			backedUp = true
		}
	}

	if !backedUp {
		command, err := provider.backupMethod(ctx, dc, c.ID)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): %w", name, provider.name, err)
		}
		backupFile := filepath.Join(backupBase,
			fmt.Sprintf("%s.%s%s", name, provider.fileExt, compressedExtension(cfg.effectiveCompression())))
		description := fmt.Sprintf("%s (%s)", name, provider.name)
		if err := writeBackup(ctx, cfg, dc, c.ID, command, backupFile, provider.fileExt, description); err != nil {
			return nil, err
		}
		dbs = nil
	}

	return &backupResult{name: name, providerType: provider.name, dbs: dbs}, nil
}

func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID[:12]
}

func writeBackup(ctx context.Context, cfg *config, dc *dockerClient, containerID string, cmd []string, backupFile, fileExt, description string) error {
	tmp, err := os.CreateTemp(filepath.Dir(backupFile), ".auto-backup-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	_, attach, err := dc.startExec(ctx, containerID, cmd, nil)
	if err != nil {
		return err
	}
	defer attach.Close()

	cw, err := newCompressWriter(tmp, cfg.effectiveCompression())
	if err != nil {
		tmp.Close()
		return err
	}

	var writer io.Writer = cw
	if cfg.showProgress {
		bar := progressbar.NewOptions64(-1,
			progressbar.OptionSetDescription(description+" "),
			progressbar.OptionShowCount(),
		)
		writer = &progressWriter{w: cw, bar: bar}
	}

	if _, err := stdcopy.StdCopy(writer, io.Discard, attach.Reader); err != nil {
		cw.Close()
		tmp.Close()
		return err
	}
	if err := cw.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	applyOwnership(tmpPath, cfg)

	fi, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		os.Remove(tmpPath)
		return fmt.Errorf("%s: 备份为空（0 字节），已丢弃", description)
	}

	if cfg.backupValidate {
		if err := validateBackupFile(cfg, tmpPath, fileExt); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("%s: 备份校验失败，已丢弃: %w", description, err)
		}
	}

	if !cfg.showProgress {
		fmt.Println(description)
	}
	return os.Rename(tmpPath, backupFile)
}

type progressWriter struct {
	w   io.Writer
	bar *progressbar.ProgressBar
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	_ = p.bar.Add(n)
	return n, err
}

func cleanOldBackups(cfg *config, now time.Time) {
	cutoff := now.Add(-time.Duration(cfg.retentionDays) * 24 * time.Hour)
	entries, err := os.ReadDir(cfg.backupDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirDate, err := time.ParseInLocation("2006-01-02", entry.Name(), time.Local)
		if err != nil {
			continue
		}
		if dirDate.Before(cutoff) {
			if err := os.RemoveAll(filepath.Join(cfg.backupDir, entry.Name())); err == nil {
				fmt.Printf("已清理旧备份: %s\n", entry.Name())
			}
		}
	}
}

func applyOwnership(path string, cfg *config) {
	if cfg.puid != 0 || cfg.pgid != 0 {
		_ = os.Chown(path, cfg.puid, cfg.pgid)
	}
}
