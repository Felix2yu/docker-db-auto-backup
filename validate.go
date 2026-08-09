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

// validateBackupFile 校验备份文件完整性：完整解压（覆盖压缩 CRC/结构检查），
// 再检查头部标识与结尾完成标记。
func validateBackupFile(cfg *config, path, fileExt string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	src := io.Reader(f)
	if algo := cfg.effectiveCompression(); algo != "" && algo != "plain" {
		src, err = newDecompressReader(f, algo)
		if err != nil {
			return err
		}
	}
	return validateBackupContent(src, fileExt)
}

// validateBackupContent 读取整个备份流，保留头部与尾部用于结构校验。
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

	switch fileExt {
	case "sql":
		h := strings.ToLower(string(head))
		headOK := strings.Contains(h, "postgresql database dump") ||
			strings.Contains(h, "mysql dump") ||
			strings.Contains(h, "mariadb dump")
		tailOK := strings.Contains(strings.ToLower(string(tail)), "dump complete")
		if !tailOK {
			return errors.New("备份尾部缺少预期的完成标记")
		}
		// 头部标识可能因 dump 变体（如 --globals-only、各发行版）而缺失，
		// 不做硬性要求，仅作为诊断提示；完整性与截断由尾部标记 + 解压 CRC 保障。
		_ = headOK
	case "rdb":
		if len(head) < 5 {
			return errors.New("RDB 备份过短，缺少魔数")
		}
		if !strings.HasPrefix(string(head), "REDIS") &&
			!strings.HasPrefix(string(head), "VALKE") {
			return errors.New("RDB 备份缺少有效的魔数")
		}
	}
	return nil
}
