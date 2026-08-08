package main

import "testing"

func TestGetBackupProvider(t *testing.T) {
	cases := map[string]string{
		"postgres":                    "postgres",
		"mysql":                       "mysql",
		"mariadb":                     "mysql",
		"linuxserver/mariadb":         "mysql",
		"tensorchord/pgvecto-rs":      "postgres",
		"nextcloud/aio-postgresql":    "postgres",
		"timescale/timescaledb":       "postgres",
		"pgvector/pgvector":           "postgres",
		"redis":                       "redis",
		"valkey":                      "redis",
		"pgautoupgrade/pgautoupgrade": "postgres",
		"immich-app/postgres":         "postgres",
	}
	for image, want := range cases {
		provider := getBackupProvider([]string{image})
		if provider == nil {
			t.Errorf("image %q: provider nil, want %s", image, want)
			continue
		}
		if provider.name != want {
			t.Errorf("image %q: got provider %s, want %s", image, provider.name, want)
		}
	}
}

func TestGetBackupProviderUnknown(t *testing.T) {
	if provider := getBackupProvider([]string{"nginx", "ghcr.io/realorangeone/db-auto-backup"}); provider != nil {
		t.Errorf("got provider %s, want nil", provider.name)
	}
}
