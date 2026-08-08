package main

import "testing"

func TestFormatTree(t *testing.T) {
	results := []backupResult{
		{
			name: "psql",
			dbs: []databaseInfo{
				{name: "appdb", isSystem: false},
				{name: "postgres", isSystem: true},
			},
		},
		{name: "redis"},
	}
	got := formatTree(results)
	want := "- psql\n    - appdb\n    - *postgres*\n- redis"
	if got != want {
		t.Errorf("formatTree:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTreeEmpty(t *testing.T) {
	if got := formatTree(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
