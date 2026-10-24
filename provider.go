package main

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
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
	// prepare 在生成备份计划时补充容器特有的元数据（产物扩展名、exec 用户等）。
	// 为空表示该 provider 无此类需求。
	prepare func(ctx context.Context, dc *dockerClient, containerID string, plan *containerPlan)
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

// nonDumpableMySQLSchemas 是 mysqldump 无法备份的库：
// information_schema 与 performance_schema 都是内存态虚拟表，
// mysqldump 自身的 --all-databases 也会跳过它们；单库模式下若逐个 dump，
// 只会因 LOCK TABLES 权限不足（1044 / 1142）而失败并污染备份结果。
var nonDumpableMySQLSchemas = map[string]bool{
	"information_schema": true,
	"performance_schema": true,
}

// safeDBName 限制可写入文件名的数据库名（C12），防止含路径分隔符导致的路径穿越。
var safeDBName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// redisBGSaveTimeoutSeconds 是等待 BGSAVE 完成的上限。
const redisBGSaveTimeoutSeconds = 120

// mysqlDumpFlags 保证导出的数据既完整又一致：
// --single-transaction 让 InnoDB 拿到跨表一致快照，同时关掉 mysqldump 默认的锁表（否则会阻塞线上写）；
// --routines / --events 默认是关闭的，不加就会静默漏掉存储过程、函数和定时事件——dump 文件本身依然合法，
// 校验和恢复演练都发现不了，只有恢复后才会发现对象不见了。
const mysqlDumpFlags = "--single-transaction --routines --events"

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
	cmd := []string{"bash", "-c", fmt.Sprintf("%s -u %s %s --all-databases", binary, shellQuote(user), mysqlDumpFlags)}
	return cmd, []string{"MYSQL_PWD=" + password}, nil
}

// redisBGSaveScript 用 BGSAVE 异步落盘（C3），再轮询等待完成。
//
// 只等 rdb_bgsave_in_progress 归零防不住陈旧数据：BGSAVE 启动失败（磁盘满、fork 失败）时该标志同样
// 立刻为 0，而 Redis 只在成功时把临时文件 rename 成 dbfilename，于是 cat 到的是上一次成功保存的旧 RDB——
// 魔数和结束标记都合法，校验通得过，却静默交付了一份陈旧备份。这里再加两道证据：
// 状态不得为 err，且 rdb_last_save_time 相比本次开始前确实前进了（除非本来就没有任何未落盘改动）。
const redisBGSaveScript = `before=$(%[1]s INFO persistence | grep -o 'rdb_last_save_time:[0-9]*' | cut -d: -f2)
%[1]s BGSAVE > /dev/null
sleep 1
i=0
while [ "$i" -lt %[2]d ]; do
  if %[1]s INFO persistence | grep -q 'rdb_bgsave_in_progress:0'; then break; fi
  i=$((i+1))
  sleep 1
done
if [ "$i" -ge %[2]d ]; then echo 'BGSAVE 超时未完成' >&2; exit 1; fi
info=$(%[1]s INFO persistence)
case "$info" in
  *rdb_last_bgsave_status:err*) echo 'BGSAVE 执行失败：rdb_last_bgsave_status:err，盘上是上一次保存的旧 RDB' >&2; exit 1 ;;
esac
after=$(printf '%%s\n' "$info" | grep -o 'rdb_last_save_time:[0-9]*' | cut -d: -f2)
changes=$(printf '%%s\n' "$info" | grep -o 'rdb_changes_since_last_save:[0-9]*' | cut -d: -f2)
if [ "$after" = "$before" ] && [ "$changes" != "0" ]; then
  echo 'BGSAVE 未产生新的 RDB（磁盘满或 fork 失败？），拒绝备份陈旧文件' >&2
  exit 1
fi
cat %[3]s
`

func redisBackupCommand(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]string, []string, error) {
	cli := "redis-cli"
	if ok, err := dc.hasBinary(ctx, containerID, "valkey-cli"); err == nil && ok {
		cli = "valkey-cli"
	}
	rdbPath := redisRDBPath(ctx, dc, containerID, cli)
	// BGSAVE 后的 sleep 1 用于规避"命令已发出但标志位尚未置 1"的竞态。
	script := fmt.Sprintf(redisBGSaveScript, cli, redisBGSaveTimeoutSeconds, shellQuote(rdbPath))
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
		if nonDumpableMySQLSchemas[line] {
			logDebug("跳过不可备份的系统库", "container", containerID, "database", line)
			continue
		}
		// C9：与 PostgreSQL 行为对齐——系统库同样备份，只是落到 system/ 子目录，
		// 避免恢复后丢失 mysql 库中的账号与权限定义。
		// --databases 让单库产物自带 CREATE DATABASE 与 USE，导入时无需事先指定目标库。
		dbs = append(dbs, database{
			name:     line,
			command:  []string{"bash", "-c", fmt.Sprintf("%s -u %s %s --databases %s", binary, shellQuote(user), mysqlDumpFlags, shellQuote(line))},
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

// ---------------------------------------------------------------------------
// SQLite（应用自导出）
// ---------------------------------------------------------------------------

// SQLite 不是服务型数据库，没有可 exec 的 dump 工具可用（应用镜像通常也不带 sqlite3 CLI），
// 因此采用「应用自导出」约定：应用提供 backup 子命令，在进程内用 SQLite 的 backup API /
// VACUUM INTO 生成事务级一致快照，并把快照写到 stdout，由本工具的现有管道落盘。
//
// 相比从容器外热拷贝 db/-wal/-shm，进程内导出是 SQLite 自己保证的一致快照：
// WAL 模式下不阻塞写入，也不需要容器内临时空间或额外二进制。

const sqliteProviderName = "sqlite"

// envSQLiteBackupCmd 是应用容器自描述的导出命令（空格分隔），例如：
//
//	BACKUP_SQLITE_CMD=shenshi backup
//
// 值应裸写（compose 映射式或整条 KEY=VALUE 加外层引号）。若写成 KEY="value"，
// 引号会存进值里，读取时由 envValue 剥掉最外层成对引号兜底。
// 设置该变量的容器会被自动识别为 sqlite provider，无需打 backup.provider 标签。
const envSQLiteBackupCmd = "BACKUP_SQLITE_CMD"

// envSQLiteBackupExt 覆盖产物文件扩展名（默认 db）。应用把数据与附件一起导出成
// zip（如 shenshi）时设为 zip，工具会按 zip 魔数校验并按 .zip 命名落盘。
const envSQLiteBackupExt = "BACKUP_SQLITE_EXT"

// envSQLiteUser 指定 exec 导出命令的用户身份（形如 1000:1000 或用户名）。
// 缺省时尝试从 PUID/PGID 推导，让导出进程与数据属主一致。
const envSQLiteUser = "BACKUP_SQLITE_USER"

// sqliteFileExtPattern 限制扩展名只能是字母数字，避免把路径分隔符带进文件名。
var sqliteFileExtPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)

// sqliteProbeTimeout 限制识别阶段的 env 探测，避免个别容器拖住整轮备份。
const sqliteProbeTimeout = 10 * time.Second

// sqliteBinaryNamePattern 约束从镜像名推导出的二进制名，防止拼接进 sh -c 时被注入。
var sqliteBinaryNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// stripEnvQuotes 剥掉环境变量值最外层的一对成对引号。
//
// compose 列表式 `- KEY="value"` 与 `docker run -e 'KEY="value"'` 都会把引号原样
// 存进值里：sh -c 收到 `"/app/app backup"` 时，整串（含空格）被当成单个命令名，
// 报 command not found。契约值是裸命令/裸扩展名/裸身份，最外层引号没有语义，剥掉。
// 不成对的引号保持原样——那不是引号包裹，是值本身的内容。
func stripEnvQuotes(v string) string {
	if n := len(v); n >= 2 && (v[0] == '"' || v[0] == '\'') && v[n-1] == v[0] {
		return v[1 : n-1]
	}
	return v
}

// envValue 读取契约环境变量：去空白 + 剥最外层成对引号。
func envValue(env map[string]string, key string) string {
	return stripEnvQuotes(strings.TrimSpace(env[key]))
}

// sqliteBackupCommand 生成 SQLite 备份命令（E 方案：应用自导出）。
//
// 命令来源优先级：
//  1. 容器环境变量 BACKUP_SQLITE_CMD（应用自描述，推荐）
//  2. 镜像名同名二进制 + backup（约定：felix2yu/shenshi → shenshi backup）
//
// 统一用 sh -c 执行：exec 的任务命令不保证做 PATH 查找，且应用镜像常把二进制
// 放在非标准目录，交给容器内的 shell 解析更可靠（应用镜像均为 alpine/debian，必有 sh）。
func sqliteBackupCommand(ctx context.Context, cfg *config, dc *dockerClient, containerID string) ([]string, []string, error) {
	if env, err := dc.containerEnv(ctx, containerID); err != nil {
		logDebug("读取容器环境变量失败，改用镜像名推导 SQLite 备份命令", "container", containerID, "error", err)
	} else if declared := envValue(env, envSQLiteBackupCmd); declared != "" {
		return []string{"sh", "-c", declared}, nil, nil
	}

	bin, err := sqliteBinaryName(ctx, dc, containerID)
	if err != nil {
		return nil, nil, err
	}
	return []string{"sh", "-c", bin + " backup"}, nil, nil
}

// sqliteBinaryName 按约定从镜像名推导应用二进制名：
// ghcr.io/felix2yu/shenshi:latest → felix2yu/shenshi → shenshi。
func sqliteBinaryName(ctx context.Context, dc *dockerClient, containerID string) (string, error) {
	names, err := dc.containerImageNames(ctx, containerID)
	if err != nil {
		return "", err
	}
	for _, name := range names {
		base := path.Base(name)
		if sqliteBinaryNamePattern.MatchString(base) {
			return base, nil
		}
	}
	return "", fmt.Errorf("无法从镜像名推导 SQLite 备份命令，请在应用容器设置 %s（如 \"myapp backup\"）", envSQLiteBackupCmd)
}

// probeSQLiteContainer 读取容器环境变量，判断它是否声明或疑似 SQLite 容器。
//
// declared：容器设置了 BACKUP_SQLITE_CMD，可直接接入备份；
// likely：环境变量里出现 .db/.sqlite 路径或 sqlite:// DSN，但未声明导出命令——
// 用于提示"疑似 SQLite 容器未被识别"，避免静默漏备份。
func probeSQLiteContainer(ctx context.Context, dc *dockerClient, containerID string) (declared, likely bool) {
	probeCtx, cancel := context.WithTimeout(ctx, sqliteProbeTimeout)
	defer cancel()

	env, err := dc.containerEnv(probeCtx, containerID)
	if err != nil {
		logDebug("读取容器环境变量失败，跳过 SQLite 识别", "container", containerID, "error", err)
		return false, false
	}
	if envValue(env, envSQLiteBackupCmd) != "" {
		return true, true
	}
	for _, v := range env {
		if sqlitePathLikeValue(v) {
			return false, true
		}
	}
	return false, false
}

// sqlitePathLikeValue 判断环境变量值是否像 SQLite 数据库路径或 DSN。
func sqlitePathLikeValue(v string) bool {
	lower := strings.ToLower(strings.TrimSpace(v))
	if lower == "" {
		return false
	}
	if strings.HasPrefix(lower, "sqlite://") || strings.HasPrefix(lower, "sqlite3://") {
		return true
	}
	for _, suffix := range []string{".db", ".sqlite", ".sqlite3"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// prepareSQLitePlan 在计划阶段解析 SQLite 容器的可选声明，
// 把产物扩展名与 exec 用户固化到计划里，后续落盘与执行直接使用。
func prepareSQLitePlan(ctx context.Context, dc *dockerClient, containerID string, plan *containerPlan) {
	if ext := sqliteFileExt(ctx, dc, containerID); ext != "" {
		plan.fileExt = ext
	}
	plan.execUser = sqliteExecUser(ctx, dc, containerID)
}

// sqliteFileExt 读取 BACKUP_SQLITE_EXT 覆盖产物扩展名（默认由 provider 给 db）。
// 应用导出 zip（数据 + 附件）时设为 zip。非法值忽略并告警，回落到默认。
func sqliteFileExt(ctx context.Context, dc *dockerClient, containerID string) string {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(envValue(env, envSQLiteBackupExt))
	if ext == "" {
		return ""
	}
	if !sqliteFileExtPattern.MatchString(ext) {
		logWarn("BACKUP_SQLITE_EXT 不合法，已忽略", "container", containerID, "value", ext)
		return ""
	}
	return ext
}

// sqliteExecUser 决定 exec 导出命令的用户身份。
//
// pg/mysql 的 dump 进程只读网络，用容器默认用户即可；SQLite 的导出进程则会碰数据目录：
// 应用停止时一个 root 身份的只读连接会创建属主为 root 的 -shm/-wal，之后降权运行的
// 应用反而打不开自己的库。因此优先让导出与数据属主同身份：
//
//  1. BACKUP_SQLITE_USER（显式声明，形如 1000:1000 或用户名）
//  2. PUID/PGID（应用以降权用户运行时的通用约定，两者都是纯数字才生效）
//  3. 留空：沿用容器默认用户（与原有行为一致）
func sqliteExecUser(ctx context.Context, dc *dockerClient, containerID string) string {
	env, err := dc.containerEnv(ctx, containerID)
	if err != nil {
		return ""
	}
	if v := envValue(env, envSQLiteUser); v != "" {
		return v
	}
	puid, pgid := envValue(env, "PUID"), envValue(env, "PGID")
	if isAllDigits(puid) && isAllDigits(pgid) {
		return puid + ":" + pgid
	}
	return ""
}

// isAllDigits 判断字符串是否为非空纯数字。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
	{
		// SQLite 没有官方镜像，不存在可靠的镜像名模式，只能靠容器标签
		// backup.provider=sqlite 或容器内环境变量 BACKUP_SQLITE_CMD 识别。
		name:         sqliteProviderName,
		patterns:     nil,
		fileExt:      "db",
		backupMethod: sqliteBackupCommand,
		prepare:      prepareSQLitePlan,
	},
}
