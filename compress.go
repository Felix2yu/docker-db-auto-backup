package main

import (
	"compress/gzip"
	"fmt"
	"io"

	"github.com/dsnet/compress/bzip2"
	"github.com/ulikunitz/xz"
)

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

// compressionAlgorithms 是本工具支持的压缩算法集合。
//
// COMPRESSION 写错（例如填了不支持的 zstd）过去只在每个容器的落盘环节报错，
// 一轮下来所有任务一起失败，日志里却看不出是配置问题；启动时用它先拒掉。
var compressionAlgorithms = map[string]bool{
	"gzip":  true,
	"lzma":  true,
	"xz":    true,
	"bz2":   true,
	"plain": true,
}

// isValidCompression 判断压缩算法是否受支持（空值表示未启用压缩）。
func isValidCompression(algo string) bool {
	return algo == "" || compressionAlgorithms[algo]
}

func compressedExtension(algorithm string) string {
	switch algorithm {
	case "gzip":
		return ".gz"
	case "lzma", "xz":
		return ".xz"
	case "bz2":
		return ".bz2"
	case "plain":
		return ""
	}
	return ""
}

func newCompressWriter(w io.Writer, algorithm string) (io.WriteCloser, error) {
	switch algorithm {
	case "gzip":
		return gzip.NewWriter(w), nil
	case "lzma", "xz":
		return xz.NewWriter(w)
	case "bz2":
		return bzip2.NewWriter(w, nil)
	case "plain":
		return nopWriteCloser{w}, nil
	}
	return nil, fmt.Errorf("未知的压缩方式 %s", algorithm)
}

func newDecompressReader(r io.Reader, algorithm string) (io.Reader, error) {
	switch algorithm {
	case "gzip":
		return gzip.NewReader(r)
	case "lzma", "xz":
		return xz.NewReader(r)
	case "bz2":
		return bzip2.NewReader(r, nil)
	case "plain":
		return r, nil
	}
	return nil, fmt.Errorf("未知的压缩方式 %s", algorithm)
}
