package main

import (
	"strings"
	"testing"
)

func TestFormatTree(t *testing.T) {
	results := []backupResult{
		{
			name:         "psql",
			providerType: "postgres",
			dbs: []databaseInfo{
				{name: "appdb", isSystem: false},
				{name: "postgres", isSystem: true},
			},
		},
		{name: "cache", providerType: "redis"},
		{name: "sql", providerType: "mysql"},
	}
	got := formatTree(results)
	want := "### PostgreSQL\n- psql\n  - appdb\n  - *postgres*\n\n### MySQL\n- sql\n\n### Redis\n- cache"
	if got != want {
		t.Errorf("formatTree:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTreeEmpty(t *testing.T) {
	if got := formatTree(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// 未知 provider 也要出现在树上，不能因为不在内置分组里就被丢掉。
func TestFormatManifestTreeGroupsUnknownProviders(t *testing.T) {
	m := &backupManifest{Containers: []containerManifest{
		{Name: "pg", Provider: "postgres", Mode: modeSingle, Files: []fileEntry{{Path: "a", Size: 10}}},
		{Name: "misc", Files: []fileEntry{{Path: "b", Size: 5, Database: "b"}}},
		{Name: "custom", Provider: "mydb", Mode: "unknown-mode", Files: []fileEntry{{Path: "c.sql"}}},
	}}
	out := formatManifestTree(m)
	for _, want := range []string{"### PostgreSQL", "### 其他", "custom", "b (5 B)"} {
		if !strings.Contains(out, want) {
			t.Errorf("树里应包含 %q:\n%s", want, out)
		}
	}
	if formatManifestTree(nil) != "" || formatManifestTree(&backupManifest{}) != "" {
		t.Error("空清单应返回空树")
	}
	if formatReport(nil, "1 秒") != "" {
		t.Error("空清单不应生成报告")
	}
}
