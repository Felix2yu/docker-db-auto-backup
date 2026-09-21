package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const manifestFileName = "manifest.json"
const manifestVersion = 1

const (
	statusSuccess = "success"
	statusPartial = "partial"
	statusFailed  = "failed"
)

type fileEntry struct {
	Path     string `json:"path"`
	Database string `json:"database,omitempty"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256,omitempty"`
	System   bool   `json:"system,omitempty"`
}

type containerManifest struct {
	Name            string      `json:"name"`
	Provider        string      `json:"provider"`
	Mode            string      `json:"mode"`
	DurationSeconds float64     `json:"duration_seconds"`
	Files           []fileEntry `json:"files"`
}

type kopiaManifestInfo struct {
	SnapshotID string `json:"snapshot_id,omitempty"`
	Pushed     bool   `json:"pushed"`
	Error      string `json:"error,omitempty"`
}

type backupManifest struct {
	Version         int                 `json:"version"`
	Date            string              `json:"date"`
	RunAt           time.Time           `json:"run_at"`
	Status          string              `json:"status"`
	DurationSeconds float64             `json:"duration_seconds"`
	Timezone        string              `json:"timezone,omitempty"`
	Kopia           *kopiaManifestInfo  `json:"kopia,omitempty"`
	Containers      []containerManifest `json:"containers"`
	Failures        []string            `json:"failures,omitempty"`
	Warnings        []string            `json:"warnings,omitempty"`
}

func (m *backupManifest) totalBytes() int64 {
	var total int64
	for _, c := range m.Containers {
		for _, f := range c.Files {
			total += f.Size
		}
	}
	return total
}

func (m *backupManifest) fileCount() int {
	var n int
	for _, c := range m.Containers {
		n += len(c.Files)
	}
	return n
}

func (m *backupManifest) containerSize(name string) (int64, bool) {
	var total int64
	found := false
	for _, c := range m.Containers {
		if c.Name != name {
			continue
		}
		found = true
		for _, f := range c.Files {
			total += f.Size
		}
	}
	return total, found
}

// runCollector 在并发备份过程中安全地汇总结果。
type runCollector struct {
	mu sync.Mutex
	m  *backupManifest
}

func newRunCollector(date string, runAt time.Time) *runCollector {
	return &runCollector{m: &backupManifest{
		Version:    manifestVersion,
		Date:       date,
		RunAt:      runAt,
		Status:     statusSuccess,
		Containers: []containerManifest{},
	}}
}

func (rc *runCollector) addContainer(c containerManifest) {
	rc.mu.Lock()
	rc.m.Containers = append(rc.m.Containers, c)
	rc.mu.Unlock()
}

func (rc *runCollector) addFailure(msg string) {
	rc.mu.Lock()
	rc.m.Failures = append(rc.m.Failures, msg)
	rc.mu.Unlock()
}

func (rc *runCollector) addWarning(msg string) {
	rc.mu.Lock()
	rc.m.Warnings = append(rc.m.Warnings, msg)
	rc.mu.Unlock()
}

func (rc *runCollector) setStatus(s string) {
	rc.mu.Lock()
	rc.m.Status = s
	rc.mu.Unlock()
}

func (rc *runCollector) setKopia(info *kopiaManifestInfo) {
	rc.mu.Lock()
	rc.m.Kopia = info
	rc.mu.Unlock()
}

func (rc *runCollector) finish(duration time.Duration, tz string) *backupManifest {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.m.DurationSeconds = duration.Seconds()
	rc.m.Timezone = tz
	sort.Slice(rc.m.Containers, func(i, j int) bool {
		return rc.m.Containers[i].Name < rc.m.Containers[j].Name
	})
	return rc.m
}

func manifestPath(dir string) string {
	return filepath.Join(dir, manifestFileName)
}

func writeManifest(dir string, m *backupManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+manifestFileName+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, manifestPath(dir))
}

func readManifest(dir string) (*backupManifest, error) {
	data, err := os.ReadFile(manifestPath(dir))
	if err != nil {
		return nil, err
	}
	var m backupManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// findPreviousManifest 找到当前日期之前最近一次成功写入的清单，用于异常检测与容量基线。
func findPreviousManifest(backupDir, currentDate string) (*backupManifest, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return nil, err
	}
	var dates []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == currentDate {
			continue
		}
		if _, err := time.ParseInLocation("2006-01-02", e.Name(), time.Local); err != nil {
			continue
		}
		if e.Name() >= currentDate {
			continue
		}
		dates = append(dates, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	for _, d := range dates {
		if m, err := readManifest(filepath.Join(backupDir, d)); err == nil {
			return m, nil
		}
	}
	return nil, nil
}

// detectAnomalies 对比上次清单，识别容器缺失与体积骤变（B9）。
func detectAnomalies(prev, cur *backupManifest, driftRatio float64) []string {
	if prev == nil {
		return nil
	}
	if driftRatio <= 0 {
		driftRatio = 0.5
	}
	var out []string

	prevNames := map[string]struct{}{}
	for _, c := range prev.Containers {
		prevNames[c.Name] = struct{}{}
	}
	curNames := map[string]struct{}{}
	for _, c := range cur.Containers {
		curNames[c.Name] = struct{}{}
	}

	var missing []string
	for name := range prevNames {
		if _, ok := curNames[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		out = append(out, fmt.Sprintf("容器 %s 上次有备份、本次缺失（可能已停止或识别失败）", name))
	}
	if len(prev.Containers) > 0 && len(cur.Containers) < len(prev.Containers) {
		out = append(out, fmt.Sprintf("备份容器数由 %d 降至 %d", len(prev.Containers), len(cur.Containers)))
	}

	for _, c := range cur.Containers {
		prevSize, ok := prev.containerSize(c.Name)
		if !ok || prevSize == 0 {
			continue
		}
		curSize, _ := cur.containerSize(c.Name)
		delta := float64(curSize-prevSize) / float64(prevSize)
		if delta <= -driftRatio {
			out = append(out, fmt.Sprintf("容器 %s 备份体积较上次下降 %.0f%%（%s → %s），请确认 dump 是否完整",
				c.Name, -delta*100, humanBytes(prevSize), humanBytes(curSize)))
		} else if delta >= driftRatio*4 {
			out = append(out, fmt.Sprintf("容器 %s 备份体积较上次增长 %.0f%%（%s → %s）",
				c.Name, delta*100, humanBytes(prevSize), humanBytes(curSize)))
		}
	}
	return out
}

func fileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// listBackupDates 返回备份目录下所有形如 YYYY-MM-DD 的日期目录（升序）。
func listBackupDates(backupDir string) ([]string, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return nil, err
	}
	var dates []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := time.ParseInLocation("2006-01-02", e.Name(), time.Local); err != nil {
			continue
		}
		dates = append(dates, e.Name())
	}
	sort.Strings(dates)
	return dates, nil
}

func statusLabel(status string) string {
	switch status {
	case statusSuccess:
		return "成功"
	case statusPartial:
		return "部分失败"
	case statusFailed:
		return "失败"
	}
	return strings.TrimSpace(status)
}
