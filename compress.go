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
