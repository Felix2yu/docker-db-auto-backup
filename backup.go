package main

import (
	"bytes"
	"context"
	"errors"
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

const (
	modeSingle       = "single"
	modeFull         = "full"
	modeFullFallback = "full-fallback"
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

// containerPlan 描述单个容器的备份计划。
type containerPlan struct {
	c         container.Summary
	name      string
	provider  *backupProvider
	mode      string
	dbs       []database
	command   []string
	execEnv   []string
	dbDir     string
	backupDir string
}

// backupTask 是最小调度单元：一个容器的一次 dump（A5）。
type backupTask struct {
	plan      *containerPlan
	db        *database
	filePath  string
	fileExt   string
	desc      string
	container string
	provider  string
	mode      string
}

// permanentError 标记不应重试的确定性错误（A2）。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var pe *permanentError
	return !errors.As(err, &pe)
}

func backup(ctx context.Context, cfg *config, dc *dockerClient, runAt time.Time) error {
	loc := cfg.location()
	runAt = runAt.In(loc)

	addCounter("db_backup_runs_total", 1)
	setGauge("db_backup_last_run_timestamp_seconds", float64(runAt.Unix()))

	hc := newHealthchecks(cfg)

	if err := os.MkdirAll(cfg.backupDir, 0o755); err != nil {
		hc.fail(err.Error())
		return err
	}

	// B3：重叠运行保护。上一次备份若尚未结束，本次直接跳过而不是并发写同一目录。
	lock, err := acquireRunLock(cfg.backupDir, 6*time.Hour)
	if err != nil {
		logWarn("跳过本次备份", "reason", err.Error())
		return err
	}
	defer lock.release()

	logInfo("开始备份", "time", runAt.Format(time.RFC3339), "timezone", loc.String())

	containers, err := dc.listContainers(ctx)
	if err != nil {
		hc.fail(err.Error())
		return err
	}

	dateDir := runAt.Format("2006-01-02")
	backupBase := filepath.Join(cfg.backupDir, dateDir)
	if err := os.MkdirAll(backupBase, 0o755); err != nil {
		hc.fail(err.Error())
		return err
	}
	applyOwnership(backupBase, cfg)

	// C7：临时文件集中在 .tmp/ 下，与备份产物隔离，避免半成品被 Kopia 快照进仓库。
	tmpDir := cfg.tmpDir()
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		hc.fail(err.Error())
		return err
	}
	cleanupStaleTempFiles(tmpDir)

	collector := newRunCollector(dateDir, runAt)

	// A8：明确区分"识别到的数据库容器"与"跳过的非数据库容器"。
	selected, excluded := selectContainers(cfg, containers)
	plans, skipped, unmatched := buildPlans(ctx, cfg, dc, selected, backupBase)
	logInfo("容器扫描完成", "total", len(containers), "selected", len(selected),
		"excluded", excluded, "database", len(plans), "skipped", skipped)
	for _, name := range unmatched {
		msg := fmt.Sprintf("容器 %s 疑似数据库但未能识别 provider，已跳过（可用 backup.provider 标签或 BACKUP_IMAGE_PATTERNS_FILE 指定）", name)
		logWarn(msg)
		collector.addWarning(msg)
	}

	if len(plans) == 0 {
		logWarn("未发现任何需要备份的数据库容器")
	}

	// B4：备份前磁盘空间预检，基于上次清单估算所需空间。
	prev, prevErr := findPreviousManifest(cfg.backupDir, dateDir)
	if prevErr != nil {
		logDebug("未找到历史备份清单，跳过容量估算", "error", prevErr)
	}
	estimated := requiredSpaceFor(prev, cfg.minFreeBytes)
	if err := checkFreeSpace(backupBase, cfg.minFreeBytes, estimated); err != nil {
		collector.setStatus(statusFailed)
		collector.addFailure(err.Error())
		logError("备份前检查未通过", "error", err)
		hc.fail(err.Error())
		writeManifestBestEffort(backupBase, collector.finish(time.Since(runAt), loc.String()), cfg)
		return err
	}

	hc.start()

	tasks := collectTasks(cfg, plans)
	results, failures := runTasks(ctx, cfg, dc, tasks)

	for _, r := range results {
		collector.addContainer(r)
	}
	for _, f := range failures {
		collector.addFailure(f)
		addCounter("db_backup_container_failures_total", 1)
	}

	status := statusSuccess
	if len(failures) > 0 {
		if len(results) == 0 {
			status = statusFailed
		} else {
			// A1：部分失败不再阻断全局——成功的产出仍然推送异地、仍然通知。
			status = statusPartial
		}
	}
	collector.setStatus(status)

	duration := time.Since(runAt)
	m := collector.finish(duration, loc.String())

	// B9：与上次清单对比，识别容器缺失与体积骤变。
	anomalies := detectAnomalies(prev, m, cfg.sizeDriftRatio)
	for _, w := range anomalies {
		logWarn("备份异常检测", "detail", w)
		collector.addWarning(w)
	}
	if len(anomalies) > 0 {
		m = collector.finish(duration, loc.String())
	}

	// 清单必须先落盘：它位于 backupBase 内，会被 Kopia 一并快照。
	writeManifestBestEffort(backupBase, m, cfg)

	// A1：Kopia 快照在部分失败时同样执行。
	if cfg.kopiaEnabled() {
		if err := runKopia(ctx, cfg, backupBase, dateDir, collector); err != nil {
			logError("Kopia 异地备份失败", "error", err)
			collector.addFailure(fmt.Sprintf("Kopia: %v", err))
			if status == statusSuccess {
				status = statusPartial
				collector.setStatus(status)
			}
		} else {
			logInfo("Kopia 快照已推送")
		}
		m = collector.finish(time.Since(runAt), loc.String())
		writeManifestBestEffort(backupBase, m, cfg)
	}

	// B7：可选的恢复演练，失败只告警不阻断。
	if cfg.restoreDrill && len(results) > 0 {
		warnings := runRestoreDrill(ctx, cfg, dc, backupBase, m, plans)
		for _, w := range warnings {
			logWarn("恢复演练", "detail", w)
			collector.addWarning(w)
		}
		if len(warnings) > 0 {
			m = collector.finish(time.Since(runAt), loc.String())
			writeManifestBestEffort(backupBase, m, cfg)
		}
	}

	if cfg.retentionDays > 0 {
		cleanOldBackups(cfg, runAt)
	}

	setGauge("db_backup_last_duration_seconds", duration.Seconds())
	setGauge("db_backup_containers_total", float64(len(results)))
	setGauge("db_backup_containers_failed_total", float64(len(failures)))
	setGauge("db_backup_files_total", float64(m.fileCount()))
	setGauge("db_backup_bytes_total", float64(m.totalBytes()))
	setGauge("db_backup_last_status", statusGauge(status))
	if status == statusFailed {
		addCounter("db_backup_failures_total", 1)
	} else {
		addCounter("db_backup_partial_failures_total", statusPartialCount(status))
		setGauge("db_backup_last_success_timestamp_seconds", float64(time.Now().Unix()))
	}

	durationStr := formatDuration(duration)
	summary := fmt.Sprintf("备份%s：%d 个容器成功，%d 个失败，共 %d 个文件 / %s，耗时 %s",
		statusLabel(status), len(results), len(failures), m.fileCount(), humanBytes(m.totalBytes()), durationStr)
	logInfo(summary, "status", status)

	body := formatReport(m, durationStr)
	if len(cfg.notifyURLs) > 0 {
		notify(ctx, cfg, cfg.notifyURLs, body)
	}

	// C10：心跳统一出口，失败原因作为正文上报。
	switch status {
	case statusSuccess:
		hc.ok(summary + "\n\n已备份容器:\n" + formatManifestTree(m))
	default:
		hc.fail(summary + "\n" + strings.Join(m.Failures, "\n"))
	}

	if status == statusFailed {
		return fmt.Errorf("%s: %s", summary, strings.Join(m.Failures, "; "))
	}
	return nil
}

func statusPartialCount(status string) float64 {
	if status == statusPartial {
		return 1
	}
	return 0
}

func statusGauge(status string) float64 {
	switch status {
	case statusSuccess:
		return 1
	case statusPartial:
		return 0.5
	}
	return 0
}

// ---------------------------------------------------------------------------
// 容器筛选（B5）
// ---------------------------------------------------------------------------

func selectContainers(cfg *config, containers []container.Summary) ([]container.Summary, int) {
	var out []container.Summary
	excluded := 0
	for _, c := range containers {
		name := containerName(c)
		if matchesAny(name, cfg.excludeContainers) {
			excluded++
			logDebug("容器已被排除", "container", name)
			continue
		}
		if len(cfg.includeContainers) > 0 && !matchesAny(name, cfg.includeContainers) {
			excluded++
			continue
		}
		if len(cfg.includeLabels) > 0 && !hasAnyLabel(c.Labels, cfg.includeLabels) {
			excluded++
			logDebug("容器未命中包含标签，已跳过", "container", name)
			continue
		}
		out = append(out, c)
	}
	return out, excluded
}

func matchesAny(name string, list []string) bool {
	for _, item := range list {
		if item == "" {
			continue
		}
		if ok, err := filepath.Match(item, name); err == nil && ok {
			return true
		}
		if item == name {
			return true
		}
	}
	return false
}

// hasAnyLabel 支持 "key" 与 "key=value" 两种写法。
func hasAnyLabel(labels map[string]string, selectors []string) bool {
	for _, sel := range selectors {
		sel = strings.TrimSpace(sel)
		if sel == "" {
			continue
		}
		if idx := strings.Index(sel, "="); idx >= 0 {
			key, value := sel[:idx], sel[idx+1:]
			if v, ok := labels[key]; ok && (value == "" || v == value) {
				return true
			}
			continue
		}
		if _, ok := labels[sel]; ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 备份计划
// ---------------------------------------------------------------------------

func buildPlans(ctx context.Context, cfg *config, dc *dockerClient, containers []container.Summary, backupBase string) ([]*containerPlan, int, []string) {
	var (
		plans     []*containerPlan
		skipped   int
		unmatched []string
	)
	for _, c := range containers {
		name := containerName(c)
		names, err := dc.containerImageNames(ctx, c.ID)
		if err != nil {
			logWarn("读取容器镜像信息失败，已跳过", "container", name, "error", err)
			skipped++
			continue
		}

		provider := getBackupProviderByLabel(dc.containerBackupProviderLabel(ctx, c.ID))
		if provider == nil {
			provider = getBackupProvider(names)
		}
		if provider == nil {
			skipped++
			if isLikelyDatabase(names) {
				unmatched = append(unmatched, name)
			} else {
				logDebug("非数据库容器，已跳过", "container", name, "images", strings.Join(names, ","))
			}
			continue
		}

		plan := &containerPlan{
			c:         c,
			name:      name,
			provider:  provider,
			mode:      modeFull,
			dbDir:     filepath.Join(backupBase, name),
			backupDir: backupBase,
		}

		if cfg.singleDBMode && provider.singleDB != nil {
			dbs, err := provider.singleDB(ctx, cfg, dc, c.ID)
			if err != nil {
				logWarn("单库模式枚举失败，回退为全库备份", "container", name, "error", err)
				plan.mode = modeFullFallback
				unmatched = append(unmatched, fmt.Sprintf("%s（单库模式回退）", name))
			} else {
				plan.mode = modeSingle
				plan.dbs = dbs
			}
		}

		if plan.mode != modeSingle {
			cmd, env, err := provider.backupMethod(ctx, cfg, dc, c.ID)
			if err != nil {
				logWarn("构造备份命令失败，已跳过", "container", name, "error", err)
				skipped++
				continue
			}
			plan.command = cmd
			plan.execEnv = env
		}
		plans = append(plans, plan)
	}
	return plans, skipped, unmatched
}

var likelyDatabaseKeywords = []string{
	"postgres", "pgvector", "timescale", "mysql", "mariadb",
	"redis", "valkey", "mongo", "cockroach", "clickhouse",
}

func isLikelyDatabase(names []string) bool {
	for _, n := range names {
		lower := strings.ToLower(n)
		for _, kw := range likelyDatabaseKeywords {
			if strings.Contains(lower, kw) {
				return true
			}
		}
	}
	return false
}

// collectTasks 把计划展开为最小调度单元（A5：单库模式下各库也进入同一 worker 池）。
func collectTasks(cfg *config, plans []*containerPlan) []backupTask {
	var tasks []backupTask
	for _, plan := range plans {
		if plan.mode == modeSingle {
			for i := range plan.dbs {
				db := plan.dbs[i]
				if err := validateDBName(db.name); err != nil {
					logWarn("数据库名不合法，已跳过", "container", plan.name, "db", db.name, "error", err)
					continue
				}
				dir := plan.dbDir
				if db.isSystem {
					dir = filepath.Join(dir, "system")
				}
				tasks = append(tasks, backupTask{
					plan:      plan,
					db:        &db,
					filePath:  filepath.Join(dir, fmt.Sprintf("%s.%s%s", db.name, plan.provider.fileExt, compressedExtension(cfg.effectiveCompression()))),
					fileExt:   plan.provider.fileExt,
					desc:      fmt.Sprintf("%s/%s (%s)", plan.name, db.name, plan.provider.name),
					container: plan.name,
					provider:  plan.provider.name,
					mode:      modeSingle,
				})
			}
			continue
		}
		tasks = append(tasks, backupTask{
			plan:      plan,
			filePath:  filepath.Join(plan.backupDir, fmt.Sprintf("%s.%s%s", plan.name, plan.provider.fileExt, compressedExtension(cfg.effectiveCompression()))),
			fileExt:   plan.provider.fileExt,
			desc:      fmt.Sprintf("%s (%s)", plan.name, plan.provider.name),
			container: plan.name,
			provider:  plan.provider.name,
			mode:      plan.mode,
		})
	}
	return tasks
}

// ---------------------------------------------------------------------------
// 并发执行
// ---------------------------------------------------------------------------

func runTasks(ctx context.Context, cfg *config, dc *dockerClient, tasks []backupTask) ([]containerManifest, []string) {
	var (
		mu       sync.Mutex
		failures []string
		agg      = map[string]*containerManifest{}
		order    []string
	)

	workers := cfg.workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}
	if workers == 0 {
		return nil, nil
	}

	// A6：并发执行时关闭交互式进度条，避免多个进度条争抢同一终端光标。
	progress := cfg.showProgress && len(tasks) == 1

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t backupTask) {
			defer func() {
				<-sem
				wg.Done()
			}()
			start := time.Now()
			err := runTaskWithRetry(ctx, cfg, dc, t, progress)
			elapsed := time.Since(start).Seconds()

			mu.Lock()
			defer mu.Unlock()
			cm, ok := agg[t.container]
			if !ok {
				cm = &containerManifest{
					Name:     t.container,
					Provider: t.provider,
					Mode:     t.mode,
					Files:    []fileEntry{},
				}
				agg[t.container] = cm
				order = append(order, t.container)
			}
			cm.DurationSeconds += elapsed
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", t.desc, err))
				logError("备份失败", "task", t.desc, "error", err)
				return
			}
			fi, statErr := os.Stat(t.filePath)
			if statErr != nil {
				failures = append(failures, fmt.Sprintf("%s: 备份文件不可用: %v", t.desc, statErr))
				return
			}
			entry := fileEntry{
				Path:   relativeTo(t.filePath, cfg.backupDir),
				Size:   fi.Size(),
				System: t.db != nil && t.db.isSystem,
			}
			if t.db != nil {
				entry.Database = t.db.name
			}
			if cfg.checksumEnabled {
				if sum, err := fileChecksum(t.filePath); err == nil {
					entry.SHA256 = sum
				} else {
					logWarn("计算校验和失败", "file", t.filePath, "error", err)
				}
			}
			cm.Files = append(cm.Files, entry)
			if !progress {
				logInfo("备份完成", "task", t.desc, "size", humanBytes(fi.Size()), "seconds", fmt.Sprintf("%.1f", elapsed))
			}
		}(t)
	}
	wg.Wait()

	var out []containerManifest
	for _, name := range order {
		cm := agg[name]
		if len(cm.Files) == 0 {
			// 该容器没有任何成功产出，不计入成功结果（失败明细已另行收集）。
			continue
		}
		out = append(out, *cm)
	}
	return out, failures
}

func runTaskWithRetry(ctx context.Context, cfg *config, dc *dockerClient, t backupTask, progress bool) error {
	attempts := cfg.backupRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			logWarn("重试备份", "task", t.desc, "attempt", i+1, "of", attempts)
			select {
			case <-time.After(cfg.retryBackoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		cmd, env := t.plan.command, t.plan.execEnv
		if t.db != nil {
			cmd, env = t.db.command, t.db.env
		}
		err := writeBackup(ctx, cfg, dc, t.plan.c.ID, cmd, env, t.filePath, t.fileExt, t.desc, progress)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
	}
	return lastErr
}

func relativeTo(path, base string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}

// ---------------------------------------------------------------------------
// 单次 dump 落盘（C2 退出码 / C7 临时目录 / A2 超时）
// ---------------------------------------------------------------------------

func writeBackup(ctx context.Context, cfg *config, dc *dockerClient, containerID string, cmd, env []string, backupFile, fileExt, description string, progress bool) error {
	if err := os.MkdirAll(filepath.Dir(backupFile), 0o755); err != nil {
		return err
	}
	// C7：临时文件统一放在 .tmp/ 下，进程被强杀后也不会污染备份目录。
	tmpDir := cfg.tmpDir()
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, ".auto-backup-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	// A2：单个 dump 的超时保护，避免挂起拖垮整轮调度。
	execCtx := ctx
	cancel := func() {}
	if cfg.backupTimeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, cfg.backupTimeout)
	}
	defer cancel()

	execID, attach, err := dc.startExec(execCtx, containerID, cmd, env)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("创建 exec 失败: %w", err)
	}
	defer attach.Close()

	cw, err := newCompressWriter(tmp, cfg.effectiveCompression())
	if err != nil {
		tmp.Close()
		return permanent(err)
	}

	var writer io.Writer = cw
	if progress {
		bar := progressbar.NewOptions64(-1,
			progressbar.OptionSetDescription(description+" "),
			progressbar.OptionShowCount(),
		)
		writer = &progressWriter{w: cw, bar: bar}
	}

	// C2：stderr 不再丢弃，用于失败诊断。
	var stderr bytes.Buffer
	_, copyErr := stdcopy.StdCopy(writer, &stderr, attach.Reader)

	// 即使 StdCopy 出错，也要先取退出码，才能给出准确诊断。
	exitCode, inspectErr := dc.execExitCode(execCtx, execID)

	if cerr := cw.Close(); cerr != nil {
		tmp.Close()
		return permanent(fmt.Errorf("关闭压缩流失败: %w", cerr))
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if execCtx.Err() != nil {
		return fmt.Errorf("备份超时（超过 %s）: %w", cfg.backupTimeout, execCtx.Err())
	}
	if errors.Is(copyErr, context.DeadlineExceeded) || errors.Is(copyErr, context.Canceled) {
		return fmt.Errorf("备份被中断: %w", copyErr)
	}
	if copyErr != nil {
		return fmt.Errorf("读取备份输出失败: %w", copyErr)
	}
	if inspectErr != nil {
		return fmt.Errorf("查询执行结果失败: %w", inspectErr)
	}

	// C2：退出码非 0 一律视为失败，杜绝"残缺 dump 被判成功"。
	if exitCode != 0 {
		return permanent(fmt.Errorf("导出命令退出码 %d%s", exitCode, stderrSummary(stderr.String())))
	}

	applyOwnership(tmpPath, cfg)

	fi, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return permanent(fmt.Errorf("备份为空（0 字节），已丢弃%s", stderrSummary(stderr.String())))
	}

	if cfg.backupValidate {
		if err := validateBackupFile(cfg, tmpPath, fileExt); err != nil {
			return permanent(fmt.Errorf("备份校验失败，已丢弃: %w", err))
		}
	}

	if progress {
		fmt.Println()
	}
	return os.Rename(tmpPath, backupFile)
}

func stderrSummary(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return "，stderr: " + s
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

// ---------------------------------------------------------------------------
// 保留清理与辅助
// ---------------------------------------------------------------------------

func cleanOldBackups(cfg *config, now time.Time) {
	cutoff := now.Add(-time.Duration(cfg.retentionDays) * 24 * time.Hour)
	entries, err := os.ReadDir(cfg.backupDir)
	if err != nil {
		logWarn("读取备份目录失败，跳过保留清理", "error", err)
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
				logInfo("已清理旧备份", "date", entry.Name())
			} else {
				logWarn("清理旧备份失败", "date", entry.Name(), "error", err)
			}
		}
	}
}

func applyOwnership(path string, cfg *config) {
	if cfg.puid != 0 || cfg.pgid != 0 {
		if err := os.Chown(path, cfg.puid, cfg.pgid); err != nil {
			logDebug("设置属主失败", "path", path, "error", err)
		}
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.2f 秒", d.Seconds())
	}
	return fmt.Sprintf("%d 分钟 %d 秒", int(d/time.Minute), int(d%time.Minute/time.Second))
}

func writeManifestBestEffort(dir string, m *backupManifest, cfg *config) {
	if err := writeManifest(dir, m); err != nil {
		logWarn("写入备份清单失败", "dir", dir, "error", err)
		return
	}
	applyOwnership(manifestPath(dir), cfg)
}

func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
}
