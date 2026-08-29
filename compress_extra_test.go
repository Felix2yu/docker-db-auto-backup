package main

import (
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/dsnet/compress/bzip2"
	"github.com/ulikunitz/xz"
)

func TestNewDecompressReader(t *testing.T) {
	// gzip
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write([]byte("hello gzip"))
	gw.Close()
	r, err := newDecompressReader(&gz, "gzip")
	if err != nil {
		t.Fatalf("gzip decompress: %v", err)
	}
	out := make([]byte, 64)
	n, _ := r.Read(out)
	if string(out[:n]) != "hello gzip" {
		t.Errorf("gzip 解压内容 = %q", string(out[:n]))
	}

	// xz
	var xb bytes.Buffer
	xw, _ := xz.NewWriter(&xb)
	xw.Write([]byte("hello xz"))
	xw.Close()
	r2, err := newDecompressReader(&xb, "xz")
	if err != nil {
		t.Fatalf("xz decompress: %v", err)
	}
	out2 := make([]byte, 64)
	n2, _ := r2.Read(out2)
	if string(out2[:n2]) != "hello xz" {
		t.Errorf("xz 解压内容 = %q", string(out2[:n2]))
	}

	// bz2
	var bb bytes.Buffer
	bw, _ := bzip2.NewWriter(&bb, nil)
	bw.Write([]byte("hello bz2"))
	bw.Close()
	r3, err := newDecompressReader(&bb, "bz2")
	if err != nil {
		t.Fatalf("bz2 decompress: %v", err)
	}
	out3 := make([]byte, 64)
	n3, _ := r3.Read(out3)
	if string(out3[:n3]) != "hello bz2" {
		t.Errorf("bz2 解压内容 = %q", string(out3[:n3]))
	}

	// plain 原样返回
	r4, err := newDecompressReader(bytes.NewReader([]byte("plain")), "plain")
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	out4 := make([]byte, 64)
	n4, _ := r4.Read(out4)
	if string(out4[:n4]) != "plain" {
		t.Errorf("plain 内容 = %q", string(out4[:n4]))
	}
}

func TestNewDecompressReaderUnknown(t *testing.T) {
	if _, err := newDecompressReader(bytes.NewReader(nil), "unknown-algo"); err == nil {
		t.Error("未知算法应返回错误")
	}
}

func TestNewCompressWriterUnknown(t *testing.T) {
	if _, err := newCompressWriter(&bytes.Buffer{}, "unknown-algo"); err == nil {
		t.Error("未知算法应返回错误")
	}
}
