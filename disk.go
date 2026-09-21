package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// checkFreeSpace 备份前的磁盘空间预检（B4）。
//
// minFreeBytes 是硬性下限；estimated 是依据历史清单估算的本次所需空间（可能为 0）。
// 两者取较大值作为要求；不满足时提前失败，避免写到一半才报错。
func checkFreeSpace(dir string, minFreeBytes, estimated int64) error {
	free, err := freeSpace(dir)
	if err != nil {
		logWarn("磁盘可用空间检查不可用，已跳过", "dir", dir, "error", err)
		return nil
	}
	need := minFreeBytes
	if estimated > need {
		need = estimated
	}
	if need <= 0 {
		return nil
	}
	if int64(free) < need {
		return fmt.Errorf("备份目录可用空间不足：需要 %s，实际可用 %s（%s）",
			humanBytes(need), humanBytes(int64(free)), dir)
	}
	logInfo("磁盘空间检查通过", "dir", dir, "free", humanBytes(int64(free)), "required", humanBytes(need))
	return nil
}

// requiredSpaceFor 依据上次清单估算本次备份所需空间：历史总量 × 安全系数。
func requiredSpaceFor(prev *backupManifest, minFreeBytes int64) int64 {
	if prev == nil {
		return minFreeBytes
	}
	estimated := int64(float64(prev.totalBytes()) * 1.2)
	if estimated < minFreeBytes {
		return minFreeBytes
	}
	return estimated
}

// cleanupStaleTempFiles 清理上次中断残留的临时文件（C7）。
func cleanupStaleTempFiles(tmpDir string) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(tmpDir, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		logWarn("已清理上次中断残留的临时文件", "dir", tmpDir, "count", removed)
	}
}
