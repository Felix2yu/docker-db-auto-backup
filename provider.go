package main

import (
	"context"
	"fmt"
	"path"
	"strings"
)

type database struct {
	name     string
	command  []string
	isSystem bool
}

type backupProvider struct {
	name         string
	patterns     []string
	fileExt      string
	backupMethod func(ctx context.Context, dc *dockerClient, containerID string) ([]string, error)
	singleDB     func(ctx context.Context, dc *dockerClient, containerID string) ([]database, error)
}

var systemDatabasesPostgres = map[string]bool{
	"postgres":  true,
	"template0": true,
	"template1": true,
}

var systemDatabasesMySQL = map[string]bool{
	"information_schema": true,
	"mysql":              true,
	"performance_schema": true,
	"sys":                true,
}

func getBackupProvider(containerNames []string) *backupProvider {
	for _, name := range containerNames {
		for _, provider := range providers {
			for _, pattern := range provider.patterns {
				ok, err := path.Match(pattern, name)
				if err == nil && ok {
					return provider
				}
			}
		}
	}
	return nil
}

func psqlBackupCommand(ctx context.Context, dc *dockerClient, containerID string) ([]string, error) {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return nil, err
	}
	return []string{"pg_dumpall", "-U", postgresUserEnv(env)}, nil
}

func mysqlBackupCommand(ctx context.Context, dc *dockerClient, containerID string) ([]string, error) {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return nil, err
	}
	auth, err := mysqlAuth(env)
	if err != nil {
		return nil, err
	}
	binary := "mysqldump"
	ok, err := dc.hasBinary(ctx, containerID, "mariadb-dump")
	if err != nil {
		return nil, err
	}
	if ok {
		binary = "mariadb-dump"
	}
	return []string{"bash", "-c", binary + " " + auth + " --all-databases"}, nil
}

func redisBackupCommand(ctx context.Context, dc *dockerClient, containerID string) ([]string, error) {
	cli := "redis-cli"
	ok, err := dc.hasBinary(ctx, containerID, "valkey-cli")
	if err != nil {
		return nil, err
	}
	if ok {
		cli = "valkey-cli"
	}
	return []string{"sh", "-c", cli + " SAVE > /dev/null && cat /data/dump.rdb"}, nil
}

func psqlSingleDB(ctx context.Context, dc *dockerClient, containerID string) ([]database, error) {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return nil, err
	}
	user := postgresUserEnv(env)
	out, err := dc.execCollect(ctx, containerID, []string{
		"psql", "-U", user, "-t", "-A", "-c", "SELECT datname FROM pg_database WHERE datallowconn ORDER BY datname",
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("列出数据库失败: %w", err)
	}

	dbs := []database{{
		name:     "globals",
		command:  []string{"pg_dumpall", "--globals-only", "-U", user},
		isSystem: true,
	}}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		dbs = append(dbs, database{
			name:     line,
			command:  []string{"pg_dump", "-U", user, "-d", line},
			isSystem: systemDatabasesPostgres[line],
		})
	}
	return dbs, nil
}

func mysqlSingleDB(ctx context.Context, dc *dockerClient, containerID string) ([]database, error) {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return nil, err
	}
	auth, err := mysqlAuth(env)
	if err != nil {
		return nil, err
	}
	binary, clientBinary := "mysqldump", "mysql"
	ok, err := dc.hasBinary(ctx, containerID, "mariadb-dump")
	if err != nil {
		return nil, err
	}
	if ok {
		binary, clientBinary = "mariadb-dump", "mariadb"
	}

	out, err := dc.execCollect(ctx, containerID, []string{"bash", "-c",
		clientBinary + " -u root " + auth + ` -e "SELECT SCHEMA_NAME FROM INFORMATION_SCHEMA.SCHEMATA ORDER BY SCHEMA_NAME" -s --skip-column-names`,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("列出数据库失败: %w", err)
	}

	var dbs []database
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if systemDatabasesMySQL[line] {
			continue
		}
		dbs = append(dbs, database{
			name:    line,
			command: []string{"bash", "-c", binary + " " + auth + " " + line},
		})
	}
	return dbs, nil
}

func postgresUserEnv(env map[string]string) string {
	if user := env["POSTGRES_USER"]; user != "" {
		return user
	}
	return "postgres"
}

func mysqlAuth(env map[string]string) (string, error) {
	if v := env["MARIADB_ROOT_PASSWORD"]; v != "" {
		return "-p$MARIADB_ROOT_PASSWORD", nil
	}
	if v := env["MYSQL_ROOT_PASSWORD"]; v != "" {
		return "-p$MYSQL_ROOT_PASSWORD", nil
	}
	return "", fmt.Errorf("找不到 MySQL root 密码")
}

var providers = []*backupProvider{
	{
		name: "postgres",
		patterns: []string{
			"postgres",
			"tensorchord/pgvecto-rs",
			"tensorchord/vchord-postgres",
			"tensorchord/vchord-suite",
			"nextcloud/aio-postgresql",
			"timescale/timescaledb*",
			"pgvector/pgvector",
			"pgautoupgrade/pgautoupgrade",
			"immich-app/postgres",
			"postgis/postgis",
			"kartoza/postgis",
		},
		fileExt:      "sql",
		backupMethod: psqlBackupCommand,
		singleDB:     psqlSingleDB,
	},
	{
		name:         "mysql",
		patterns:     []string{"mysql", "mariadb", "linuxserver/mariadb"},
		fileExt:      "sql",
		backupMethod: mysqlBackupCommand,
		singleDB:     mysqlSingleDB,
	},
	{
		name:         "redis",
		patterns:     []string{"redis", "valkey", "valkey/valkey", "valkey*"},
		fileExt:      "rdb",
		backupMethod: redisBackupCommand,
	},
}
