package main

import (
	"reflect"
	"testing"
)

func TestGetContainerNames(t *testing.T) {
	cases := []struct {
		tag  string
		name string
	}{
		{"postgres:14-alpine", "postgres"},
		{"docker.io/postgres:14-alpine", "postgres"},
		{"ghcr.io/realorangeone/db-auto-backup:latest", "realorangeone/db-auto-backup"},
		{"theorangeone/db-auto-backup:latest:latest", "theorangeone/db-auto-backup"},
		{"lscr.io/linuxserver/mariadb:latest", "linuxserver/mariadb"},
		{"docker.io/library/postgres:14-alpine", "postgres"},
		{"library/postgres:14-alpine", "postgres"},
		{"pgautoupgrade/pgautoupgrade:15-alpine", "pgautoupgrade/pgautoupgrade"},
		{"ghcr.io/immich-app/postgres:17-vectorchord0.3.0-pgvectors0.3.0", "immich-app/postgres"},
	}
	for _, c := range cases {
		got := imageNamesFromTags([]string{c.tag})
		if !reflect.DeepEqual(got, []string{c.name}) {
			t.Errorf("tag %q: got %v, want [%s]", c.tag, got, c.name)
		}
	}
}

func TestGetContainerNamesDeduplicates(t *testing.T) {
	got := imageNamesFromTags([]string{"postgres:14-alpine", "postgres:latest"})
	if !reflect.DeepEqual(got, []string{"postgres"}) {
		t.Errorf("got %v, want [postgres]", got)
	}
}
