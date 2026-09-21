package main

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
)

type database struct {
	name     string
	command  []string
	env      []string
	isSystem bool
}

type backupProvider struct {
	name         string
	patterns     []string
	fileExt      string
	backupMethod func(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]string, []string, error)
	singleDB     func(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]database, error)
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

// safeDBName 限制可写入文件名的数据库名（C12），防止含路径分隔符导致的路径穿越。
var safeDBName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// redisBGSaveTimeoutSeconds 是等待 BGSAVE 完成的上限。
const redisBGSaveTimeoutSeconds = 120

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

// getBackupProviderByLabel 处理容器上显式声明的 backup.provider 标签，优先级最高。
func getBackupProviderByLabel(label string) *backupProvider {
	if label == "" {
		return nil
	}
	return providerByName(strings.ToLower(strings.TrimSpace(label)))
}

func psqlBackupCommand(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]string, []string, error) {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return nil, nil, err
	}
	user := postgresUser(cfg, env)
	return []string{"pg_dumpall", "-U", user}, postgresExecEnv(cfg, env), nil
}

func mysqlBackupCommand(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]string, []string, error) {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return nil, nil, err
	}
	user, password, err := mysqlCredentials(cfg, env)
	if err != nil {
		return nil, nil, err
	}
	binary := "mysqldump"
	if ok, err := dc.hasBinary(ctx, containerID, "mariadb-dump"); err == nil && ok {
		binary = "mariadb-dump"
	}
	// C4：密码不再拼进命令行，改由 MYSQL_PWD 环境注入，
	// 既避免出现在进程列表中，也杜绝了 -p$VAR 展开失败后交互式等待密码的挂死。
	cmd := []string{"bash", "-c", fmt.Sprintf("%s -u %s --all-databases", binary, shellQuote(user))}
	return cmd, []string{"MYSQL_PWD=" + password}, nil
}

func redisBackupCommand(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]string, []string, error) {
	cli := "redis-cli"
	if ok, err := dc.hasBinary(ctx, containerID, "valkey-cli"); err == nil && ok {
		cli = "valkey-cli"
	}
	rdbPath := redisRDBPath(ctx, dc, containerID, cli)
	// C3：用 BGSAVE 替代同步 SAVE，避免阻塞 Redis 主线程；
	// 再轮询 INFO persistence 等待落盘完成，超时则报错而不是备份一个陈旧文件。
	// BGSAVE 后的 sleep 1 用于规避"命令已发出但标志位尚未置 1"的竞态。
	script := fmt.Sprintf(
		"%s BGSAVE > /dev/null; sleep 1; i=0; while [ $i -lt %d ]; do if %s INFO persistence | grep -q 'rdb_bgsave_in_progress:0'; then break; fi; i=$((i+1)); sleep 1; done; if [ $i -ge %d ]; then echo 'BGSAVE 超时未完成' >&2; exit 1; fi; cat %s",
		cli, redisBGSaveTimeoutSeconds, cli, redisBGSaveTimeoutSeconds, shellQuote(rdbPath))
	return []string{"sh", "-c", script}, nil, nil
}

// redisRDBPath 通过 CONFIG GET 动态获取 RDB 落盘路径，失败时回退到 /data/dump.rdb。
func redisRDBPath(ctx context.Context, dc *dockerClient, containerID, cli string) string {
	dir := "/data"
	filename := "dump.rdb"

	if out, err := dc.execCollect(ctx, containerID, []string{cli, "CONFIG", "GET", "dir"}, nil); err == nil {
		if v := parseConfigGet(out); v != "" {
			dir = v
		}
	} else {
		logDebug("CONFIG GET dir 不可用，使用默认目录", "container", containerID, "error", err)
	}
	if out, err := dc.execCollect(ctx, containerID, []string{cli, "CONFIG", "GET", "dbfilename"}, nil); err == nil {
		if v := parseConfigGet(out); v != "" {
			filename = v
		}
	} else {
		logDebug("CONFIG GET dbfilename 不可用，使用默认文件名", "container", containerID, "error", err)
	}
	return strings.TrimSuffix(dir, "/") + "/" + filename
}

// parseConfigGet 解析 "CONFIG GET key" 的两行输出（键名 + 值）。
func parseConfigGet(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return ""
	}
	return strings.TrimSpace(lines[1])
}

func psqlSingleDB(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]database, error) {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return nil, err
	}
	user := postgresUser(cfg, env)
	execEnv := postgresExecEnv(cfg, env)
	out, err := dc.execCollect(ctx, containerID, []string{
		"psql", "-U", user, "-t", "-A", "-c",
		"SELECT datname FROM pg_database WHERE datallowconn AND datname NOT IN ('template0', 'template1') ORDER BY datname",
	}, execEnv)
	if err != nil {
		return nil, fmt.Errorf("列出数据库失败: %w", err)
	}

	dbs := []database{{
		name:     "globals",
		command:  []string{"pg_dumpall", "--globals-only", "-U", user},
		env:      execEnv,
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
			env:      execEnv,
			isSystem: systemDatabasesPostgres[line],
		})
	}
	return dbs, nil
}

func mysqlSingleDB(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]database, error) {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return nil, err
	}
	user, password, err := mysqlCredentials(cfg, env)
	if err != nil {
		return nil, err
	}
	binary, clientBinary := "mysqldump", "mysql"
	if ok, err := dc.hasBinary(ctx, containerID, "mariadb-dump"); err == nil && ok {
		binary, clientBinary = "mariadb-dump", "mariadb"
	}
	execEnv := []string{"MYSQL_PWD=" + password}

	out, err := dc.execCollect(ctx, containerID, []string{"bash", "-c",
		fmt.Sprintf("%s -u %s -e \"SELECT SCHEMA_NAME FROM INFORMATION_SCHEMA.SCHEMATA ORDER BY SCHEMA_NAME\" -s --skip-column-names",
			clientBinary, shellQuote(user)),
	}, execEnv)
	if err != nil {
		return nil, fmt.Errorf("列出数据库失败: %w", err)
	}

	var dbs []database
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// C9：与 PostgreSQL 行为对齐——系统库同样备份，只是落到 system/ 子目录，
		// 避免恢复后丢失 mysql 库中的账号与权限定义。
		dbs = append(dbs, database{
			name:     line,
			command:  []string{"bash", "-c", fmt.Sprintf("%s -u %s %s", binary, shellQuote(user), shellQuote(line))},
			env:      execEnv,
			isSystem: systemDatabasesMySQL[line],
		})
	}
	return dbs, nil
}

// postgresUser 按优先级解析连接用户（C5）：备份专用账号 > POSTGRES_USER > PGUSER > postgres。
func postgresUser(cfg *config, env map[string]string) string {
	if cfg != nil && cfg.pgUser != "" {
		return cfg.pgUser
	}
	if u := env["POSTGRES_USER"]; u != "" {
		return u
	}
	if u := env["PGUSER"]; u != "" {
		return u
	}
	return "postgres"
}

// postgresExecEnv 提供 PGPASSWORD，供无 socket 认证的远端连接使用。
func postgresExecEnv(cfg *config, env map[string]string) []string {
	if cfg != nil && cfg.pgUser != "" {
		return nil
	}
	if p := env["POSTGRES_PASSWORD"]; p != "" {
		return []string{"PGPASSWORD=" + p}
	}
	if p := env["PGPASSWORD"]; p != "" {
		return []string{"PGPASSWORD=" + p}
	}
	return nil
}

// mysqlCredentials 解析 MySQL/MariaDB 连接凭据（C5）。
// 找不到密码时直接返回错误——绝不退化成等待交互输入，避免出现永久挂起。
func mysqlCredentials(cfg *config, env map[string]string) (user, password string, err error) {
	user = "root"
	if cfg != nil && cfg.mysqlUser != "" {
		user = cfg.mysqlUser
	} else if u := env["MYSQL_USER"]; u != "" {
		user = u
	}

	password = env["MARIADB_ROOT_PASSWORD"]
	if password == "" {
		password = env["MYSQL_ROOT_PASSWORD"]
	}
	if password == "" {
		password = env["MYSQL_PASSWORD"]
	}
	if cfg != nil && cfg.mysqlPassword != "" {
		password = cfg.mysqlPassword
	}
	if cfg != nil && cfg.mysqlUser != "" && cfg.mysqlPassword == "" {
		return "", "", fmt.Errorf("已指定 MYSQL_BACKUP_USER=%s 但未提供 MYSQL_BACKUP_PASSWORD", cfg.mysqlUser)
	}
	if password == "" {
		return "", "", fmt.Errorf("找不到 MySQL 凭据（可在容器中提供 MYSQL_ROOT_PASSWORD / MARIADB_ROOT_PASSWORD，或设置 MYSQL_BACKUP_USER 与 MYSQL_BACKUP_PASSWORD）")
	}
	return user, password, nil
}

// shellQuote 用单引号包裹字符串，供拼接进 sh -c 的参数使用。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"$&|;<>()\\`*?[]#~=%") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func validateDBName(name string) error {
	if name == "" {
		return fmt.Errorf("数据库名为空")
	}
	if !safeDBName.MatchString(name) {
		return fmt.Errorf("数据库名 %q 含不安全字符，已跳过", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("数据库名 %q 非法", name)
	}
	return nil
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
			"bitnami/postgresql",
			"zabbix/zabbix-server-pgsql",
		},
		fileExt:      "sql",
		backupMethod: psqlBackupCommand,
		singleDB:     psqlSingleDB,
	},
	{
		name: "mysql",
		patterns: []string{
			"mysql",
			"mariadb",
			"linuxserver/mariadb",
			"bitnami/mariadb",
			"bitnami/mysql",
			"zabbix/zabbix-server-mysql",
		},
		fileExt:      "sql",
		backupMethod: mysqlBackupCommand,
		singleDB:     mysqlSingleDB,
	},
	{
		name:         "redis",
		patterns:     []string{"redis", "valkey", "valkey/valkey", "valkey*", "bitnami/redis", "bitnami/valkey"},
		fileExt:      "rdb",
		backupMethod: redisBackupCommand,
	},
}
