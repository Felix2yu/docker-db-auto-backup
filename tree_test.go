package main

import "testing"

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
	want := "### PostgreSQL\n\n- psql\n    - appdb\n    - *postgres*\n\n---\n\n### MySQL\n\n- sql\n\n---\n\n### Redis\n\n- cache"
	if got != want {
		t.Errorf("formatTree:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTreeEmpty(t *testing.T) {
	if got := formatTree(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
