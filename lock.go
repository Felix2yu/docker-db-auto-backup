package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const lockFileName = ".backup.lock"

// runLock 提供跨平台的"单实例运行"保护（B3）。
//
// 这里不使用 flock，因为 Windows 上没有对应实现；改为创建一个带 O_EXCL 的锁文件，
// 并在获取失败时通过锁文件的修改时间判断持有者是否已经僵死（例如容器被强杀后
// 锁文件残留）。超过 staleAfter 的锁文件会被视为可安全接管。
type runLock struct {
	path string
	held bool
}

func acquireRunLock(backupDir string, staleAfter time.Duration) (*runLock, error) {
	if backupDir == "" {
		return &runLock{}, nil
	}
	if staleAfter <= 0 {
		staleAfter = 6 * time.Hour
	}
	path := filepath.Join(backupDir, lockFileName)
	l := &runLock{path: path}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintf(f, "pid=%d started=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
		f.Close()
		l.held = true
		return l, nil
	}
	if !os.IsExist(err) {
		return nil, fmt.Errorf("无法创建运行锁 %s: %w", path, err)
	}

	// 锁已存在：判断是否为僵死锁
	info, statErr := os.Stat(path)
	if statErr == nil && time.Since(info.ModTime()) > staleAfter {
		logWarn("发现僵死的运行锁，已接管", "path", path, "age", time.Since(info.ModTime()).String())
		os.Remove(path)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid=%d started=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
			f.Close()
			l.held = true
			return l, nil
		}
	}
	return nil, fmt.Errorf("已有备份实例正在运行（锁文件 %s），跳过本次执行", path)
}

func (l *runLock) release() {
	if l == nil || !l.held || l.path == "" {
		return
	}
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		logWarn("释放运行锁失败", "path", l.path, "error", err)
	}
	l.held = false
}
