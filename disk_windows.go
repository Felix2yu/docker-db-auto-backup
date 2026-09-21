//go:build windows

package main

import "errors"

// freeSpace 在 Windows 上不做实现，磁盘预检会自动跳过。
func freeSpace(path string) (uint64, error) {
	return 0, errors.New("当前平台不支持磁盘空间检查")
}
