package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	headBuffLen = 8 << 10
	tailBuffLen = 64 << 10
)

// validateBackupFile 校验备份文件完整性（A4）。
//
// 压缩格式：完整解压一遍，借解压器的 CRC/结构检查发现损坏。
// plain 格式：解压没有额外收益，改为 Seek 直接读首尾，避免大库场景下 IO 翻倍。
func validateBackupFile(cfg *config, path, fileExt string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if algo := cfg.effectiveCompression(); algo != "" && algo != "plain" {
		src, err := newDecompressReader(f, algo)
		if err != nil {
			return err
		}
		return validateBackupContent(src, fileExt)
	}
	return validatePlainBackup(f, fileExt)
}

// validatePlainBackup 只在文件首尾各读一段，用于未压缩备份的结构校验。
func validatePlainBackup(f *os.File, fileExt string) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if size == 0 {
		return errors.New("备份为空")
	}

	headBuf := make([]byte, headBuffLen)
	n, err := f.ReadAt(headBuf, 0)
	if err != nil && err != io.EOF {
		return fmt.Errorf("读取备份头部失败: %w", err)
	}
	head := headBuf[:n]
	if len(head) == 0 {
		return errors.New("备份头部为空")
	}

	tailStart := size - int64(tailBuffLen)
	if tailStart < 0 {
		tailStart = 0
	}
	tailBuf := make([]byte, size-tailStart)
	n, err = f.ReadAt(tailBuf, tailStart)
	if err != nil && err != io.EOF {
		return fmt.Errorf("读取备份尾部失败: %w", err)
	}
	tail := tailBuf[:n]
	if len(tail) == 0 {
		return errors.New("备份尾部为空")
	}

	return checkBackupStructure(head, tail, size, fileExt)
}

// validateBackupContent 读取整个备份流，保留头部与尾部用于结构校验。
// 仅用于压缩格式（此时全量解压本身即是完整性检查）。
func validateBackupContent(r io.Reader, fileExt string) error {
	head := make([]byte, 0, headBuffLen)
	tail := make([]byte, 0, tailBuffLen)
	buf := make([]byte, 64<<10)
	total := int64(0)

	for {
		n, err := r.Read(buf)
		if n > 0 {
			total += int64(n)
			chunk := buf[:n]

			if len(head) < headBuffLen {
				need := headBuffLen - len(head)
				if len(chunk) > need {
					head = append(head, chunk[:need]...)
				} else {
					head = append(head, chunk...)
				}
			}

			tail = append(tail, chunk...)
			if len(tail) > tailBuffLen {
				tail = tail[len(tail)-tailBuffLen:]
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取备份内容失败: %w", err)
		}
	}

	if total == 0 {
		return errors.New("备份为空")
	}
	return checkBackupStructure(head, tail, total, fileExt)
}

func checkBackupStructure(head, tail []byte, total int64, fileExt string) error {
	if total == 0 {
		return errors.New("备份为空")
	}

	switch fileExt {
	case "sql":
		tailOK := strings.Contains(strings.ToLower(string(tail)), "dump complete")
		if !tailOK {
			return errors.New("备份尾部缺少预期的完成标记，可能被截断")
		}
		// 头部标识可能因 dump 变体（如 --globals-only、各发行版）而缺失，
		// 不做硬性要求，仅作为诊断提示；完整性与截断由尾部标记 + 解压 CRC 保障。
	case "rdb":
		if len(head) < 5 {
			return errors.New("RDB 备份过短，缺少魔数")
		}
		if !strings.HasPrefix(string(head), "REDIS") &&
			!strings.HasPrefix(string(head), "VALKE") {
			return errors.New("RDB 备份缺少有效的魔数")
		}
		// RDB 约定以 0xFF 结束，缺失说明文件被截断。
		if len(tail) > 0 && tail[len(tail)-1] != 0xFF {
			return errors.New("RDB 备份缺少结束标记 0xFF，可能被截断")
		}
	}
	return nil
}
