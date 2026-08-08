package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/dsnet/compress/bzip2"
	"github.com/ulikunitz/xz"
)

func TestCompressedExtension(t *testing.T) {
	cases := []struct {
		algorithm, want string
	}{
		{"gzip", ".gz"},
		{"lzma", ".xz"},
		{"xz", ".xz"},
		{"bz2", ".bz2"},
		{"plain", ""},
		{"unknown", ""},
	}
	for _, c := range cases {
		if got := compressedExtension(c.algorithm); got != c.want {
			t.Errorf("compressedExtension(%q): got %q, want %q", c.algorithm, got, c.want)
		}
	}
}

func TestCompressRoundtrip(t *testing.T) {
	payload := []byte("hello quicktrip\ndata-auto-backup\n")
	decompressors := map[string]func([]byte) ([]byte, error){
		"gzip": func(b []byte) ([]byte, error) {
			r, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		},
		"lzma": xzDecode,
		"xz":   xzDecode,
		"bz2": func(b []byte) ([]byte, error) {
			r, err := bzip2.NewReader(bytes.NewReader(b), nil)
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		},
		"plain": func(b []byte) ([]byte, error) { return b, nil },
	}

	for _, algorithm := range []string{"gzip", "lzma", "xz", "bz2", "plain"} {
		t.Run(algorithm, func(t *testing.T) {
			var buf bytes.Buffer
			w, err := newCompressWriter(&buf, algorithm)
			if err != nil {
				t.Fatalf("newCompressWriter: %v", err)
			}
			if _, err := w.Write(payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			got, err := decompressors[algorithm](buf.Bytes())
			if err != nil {
				t.Fatalf("decompress: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("roundtrip mismatch: got %q, want %q", got, payload)
			}
		})
	}
}

func xzDecode(b []byte) ([]byte, error) {
	r, err := xz.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestCompressWriterUnknown(t *testing.T) {
	var buf bytes.Buffer
	if _, err := newCompressWriter(&buf, "nope"); err == nil {
		t.Error("expected error for unknown algorithm")
	}
}
