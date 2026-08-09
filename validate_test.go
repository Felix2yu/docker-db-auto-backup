package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateBackupContentPostgres(t *testing.T) {
	r := strings.NewReader("-- PostgreSQL database dump (16.3)\n\nCREATE TABLE t (id int);\n\n-- PostgreSQL database dump complete\n")
	if err := validateBackupContent(r, "sql"); err != nil {
		t.Errorf("valid postgres dump rejected: %v", err)
	}
}

func TestValidateBackupContentMySQL(t *testing.T) {
	r := strings.NewReader("-- MySQL dump 10.13  Distrib 8.4.2\n\nCREATE TABLE t (id int);\n-- Dump completed on 2026-08-09 04:00:00\n")
	if err := validateBackupContent(r, "sql"); err != nil {
		t.Errorf("valid mysql dump rejected: %v", err)
	}
}

func TestValidateBackupContentMariaDB(t *testing.T) {
	r := strings.NewReader("-- MariaDB dump 10.19  Distrib 11.4.2\n\nCREATE TABLE t (id int);\n-- MariaDB dump complete\n")
	if err := validateBackupContent(r, "sql"); err != nil {
		t.Errorf("valid mariadb dump rejected: %v", err)
	}
}

func TestValidateBackupContentSQLVariants(t *testing.T) {
	cases := map[string]struct {
		content string
		ext     string
	}{
		"globals 变体无头部标识": {
			"-- Global objects\nCREATE ROLE app;\nALTER ROLE app WITH LOGIN;\n-- PostgreSQL database cluster dump complete\n",
			"sql",
		},
		"pg_dumpall 全库": {
			"--\n-- PostgreSQL database cluster dump\n--\nCREATE TABLE x (id int);\n--\n-- PostgreSQL database cluster dump complete\n--\n",
			"sql",
		},
		"仅尾完成标记": {
			"\x01SET client_encoding = 'UTF8';\nCREATE ROLE r;\n-- dump complete\n",
			"sql",
		},
	}
	for name, c := range cases {
		if err := validateBackupContent(strings.NewReader(c.content), c.ext); err != nil {
			t.Errorf("%s: expected ok, got %v", name, err)
		}
	}
}

func TestValidateBackupRDB(t *testing.T) {
	if err := validateBackupContent(strings.NewReader("REDIS0015...."), "rdb"); err != nil {
		t.Errorf("valid redis rdb rejected: %v", err)
	}
	if err := validateBackupContent(strings.NewReader("VALKE0015...."), "rdb"); err != nil {
		t.Errorf("valid valkey rdb rejected: %v", err)
	}
}

func TestValidateBackupRejectsCorrupt(t *testing.T) {
	cases := map[string]struct {
		content string
		ext     string
	}{
		"非 dump 内容": {"<html>error page</html>", "sql"},
		"截断无完成标记":   {"-- PostgreSQL database dump (16.3)\nCREATE TABLE t", "sql"},
		"空备份":       {"", "sql"},
		"rdb 魔数错误":  {"NOTARD 0015...", "rdb"},
		"rdb 过短":    {"RED", "rdb"},
	}
	for name, c := range cases {
		if err := validateBackupContent(strings.NewReader(c.content), c.ext); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestValidateBackupFileGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.sql.gz")
	cfg := &config{compression: "gzip"}

	content := "-- PostgreSQL database dump (16.3)\nCREATE TABLE t (id int);\n-- PostgreSQL database dump complete\n"

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(content))
	zw.Close()

	trunc := buf.Bytes()[:buf.Len()-4]
	if err := os.WriteFile(path, trunc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupFile(cfg, path, "sql"); err == nil {
		t.Error("truncated gzip should fail validation")
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupFile(cfg, path, "sql"); err != nil {
		t.Errorf("intact gzip rejected: %v", err)
	}
}
