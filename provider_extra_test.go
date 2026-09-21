package main

import (
	"context"
	"strings"
	"testing"
)

func TestPostgresUser(t *testing.T) {
	if got := postgresUser(nil, map[string]string{}); got != "postgres" {
		t.Errorf("默认应为 postgres, got %q", got)
	}
	if got := postgresUser(nil, map[string]string{"POSTGRES_USER": "alice"}); got != "alice" {
		t.Errorf("应为 alice, got %q", got)
	}
	if got := postgresUser(nil, map[string]string{"PGUSER": "bob"}); got != "bob" {
		t.Errorf("PGUSER 应被识别, got %q", got)
	}
	// 备份专用账号优先级最高（C5）
	if got := postgresUser(&config{pgUser: "backup"}, map[string]string{"POSTGRES_USER": "alice"}); got != "backup" {
		t.Errorf("备份专用账号应优先, got %q", got)
	}
}

func TestMysqlCredentials(t *testing.T) {
	u, p, err := mysqlCredentials(nil, map[string]string{"MARIADB_ROOT_PASSWORD": "p1"})
	if err != nil || p != "p1" || u != "root" {
		t.Errorf("got user=%q pass=%q err=%v, want root/p1", u, p, err)
	}
	_, p, err = mysqlCredentials(nil, map[string]string{"MYSQL_ROOT_PASSWORD": "p2"})
	if err != nil || p != "p2" {
		t.Errorf("got pass=%q err=%v, want p2", p, err)
	}
	if _, _, err := mysqlCredentials(nil, map[string]string{}); err == nil {
		t.Error("缺少密码时应返回错误（不得退化为交互式等待）")
	}
	// 指定备份账号但未提供密码时应报错（C4）
	if _, _, err := mysqlCredentials(&config{mysqlUser: "bk"}, map[string]string{"MYSQL_ROOT_PASSWORD": "x"}); err == nil {
		t.Error("指定了备份账号但无密码时应报错")
	}
	u, p, err = mysqlCredentials(&config{mysqlUser: "bk", mysqlPassword: "secret"}, map[string]string{"MYSQL_ROOT_PASSWORD": "x"})
	if err != nil || u != "bk" || p != "secret" {
		t.Errorf("备份专用凭据应优先: user=%q pass=%q err=%v", u, p, err)
	}
}

func TestPsqlBackupCommand(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	cmd, env, err := psqlBackupCommand(context.Background(), nil, dc, "c1")
	if err != nil {
		t.Fatalf("psqlBackupCommand: %v", err)
	}
	if len(cmd) != 3 || cmd[0] != "pg_dumpall" || cmd[2] != "postgres" {
		t.Errorf("unexpected cmd: %v", cmd)
	}
	if len(env) != 1 || env[0] != "PGPASSWORD=secret" {
		t.Errorf("应注入 PGPASSWORD, got %v", env)
	}
}

func TestMysqlBackupCommand(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	cmd, env, err := mysqlBackupCommand(context.Background(), nil, dc, "c1")
	if err != nil {
		t.Fatalf("mysqlBackupCommand: %v", err)
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "mysqldump") || !strings.Contains(joined, "--all-databases") {
		t.Errorf("unexpected cmd: %v", cmd)
	}
	// C4：密码不得出现在命令行中
	if strings.Contains(joined, "-p") || strings.Contains(joined, "rootpass") {
		t.Errorf("密码不应出现在命令行: %v", cmd)
	}
	if len(env) != 1 || env[0] != "MYSQL_PWD=rootpass" {
		t.Errorf("密码应通过 MYSQL_PWD 注入, got %v", env)
	}
}

func TestMysqlBackupCommandMariadb(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "which" {
			return []byte("/usr/bin/mariadb-dump"), nil, 0
		}
		if cmd[0] == "env" {
			return []byte("MARIADB_ROOT_PASSWORD=root\n"), nil, 0
		}
		return nil, nil, 0
	}
	dc := newFakeDockerClient(fake)
	cmd, _, err := mysqlBackupCommand(context.Background(), nil, dc, "c1")
	if err != nil {
		t.Fatalf("mysqlBackupCommand: %v", err)
	}
	if !strings.Contains(strings.Join(cmd, " "), "mariadb-dump") {
		t.Errorf("存在 mariadb-dump 时应优先使用: %v", cmd)
	}
}

func TestRedisBackupCommand(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	cmd, _, err := redisBackupCommand(context.Background(), nil, dc, "c1")
	if err != nil {
		t.Fatalf("redisBackupCommand: %v", err)
	}
	joined := strings.Join(cmd, " ")
	// C3：必须使用 BGSAVE，绝不能是同步 SAVE
	if !strings.Contains(joined, "BGSAVE") {
		t.Errorf("应使用 BGSAVE: %v", cmd)
	}
	if strings.Contains(joined, "redis-cli SAVE") || strings.Contains(joined, "valkey-cli SAVE") {
		t.Errorf("同步 SAVE 会阻塞主线程，不应使用: %v", cmd)
	}
	if !strings.Contains(joined, "/data/dump.rdb") {
		t.Errorf("应 cat 动态获取的 RDB 路径: %v", cmd)
	}
}

func TestRedisBackupCommandValkey(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "which" {
			return []byte("/usr/bin/valkey-cli"), nil, 0
		}
		return nil, nil, 0
	}
	dc := newFakeDockerClient(fake)
	cmd, _, err := redisBackupCommand(context.Background(), nil, dc, "c1")
	if err != nil {
		t.Fatalf("redisBackupCommand: %v", err)
	}
	if !strings.Contains(strings.Join(cmd, " "), "valkey-cli BGSAVE") {
		t.Errorf("存在 valkey-cli 时应优先使用: %v", cmd)
	}
}

// TestRedisRDBPathDynamic 覆盖自定义 dir/dbfilename 的场景。
func TestRedisRDBPathDynamic(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if joined := strings.Join(cmd, " "); strings.Contains(joined, "CONFIG GET dir") {
			return []byte("dir\n/var/lib/redis\n"), nil, 0
		}
		if joined := strings.Join(cmd, " "); strings.Contains(joined, "CONFIG GET dbfilename") {
			return []byte("dbfilename\ncustom.rdb\n"), nil, 0
		}
		return nil, nil, 0
	}
	dc := newFakeDockerClient(fake)
	if got := redisRDBPath(context.Background(), dc, "c1", "redis-cli"); got != "/var/lib/redis/custom.rdb" {
		t.Errorf("RDB 路径 = %q, want /var/lib/redis/custom.rdb", got)
	}
}

func TestPsqlSingleDB(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	dbs, err := psqlSingleDB(context.Background(), nil, dc, "c1")
	if err != nil {
		t.Fatalf("psqlSingleDB: %v", err)
	}
	if len(dbs) == 0 {
		t.Fatal("应列出数据库")
	}
	for _, d := range dbs {
		if d.name == "globals" && !d.isSystem {
			t.Error("globals 应标记为系统库")
		}
	}
}

func TestPsqlSingleDBEnvError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		return nil, []byte("err"), 1
	}
	dc := newFakeDockerClient(fake)
	if _, err := psqlSingleDB(context.Background(), nil, dc, "c1"); err == nil {
		t.Error("env 执行失败应返回错误")
	}
}

func TestPsqlSingleDBListError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "env" {
			return []byte("POSTGRES_USER=postgres\n"), nil, 0
		}
		return nil, []byte("err"), 1
	}
	dc := newFakeDockerClient(fake)
	if _, err := psqlSingleDB(context.Background(), nil, dc, "c1"); err == nil {
		t.Error("列出数据库失败应返回错误")
	}
}

func TestMysqlSingleDB(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	dbs, err := mysqlSingleDB(context.Background(), nil, dc, "c1")
	if err != nil {
		t.Fatalf("mysqlSingleDB: %v", err)
	}
	if len(dbs) == 0 {
		t.Fatal("应列出数据库")
	}
	// C9：系统库与 PostgreSQL 行为对齐——同样导出，只是标记为系统库
	found := false
	for _, d := range dbs {
		if d.name == "mysql" {
			found = true
			if !d.isSystem {
				t.Error("mysql 库应标记为系统库")
			}
		}
	}
	if !found {
		t.Error("mysql 系统库应被纳入备份（恢复到新实例需要账号与权限）")
	}
	// information_schema / performance_schema 是内存虚拟库，mysqldump 无法备份，
	// 强行 dump 只会因 LOCK TABLES 权限（1044 / 1142）失败。
	for _, d := range dbs {
		if d.name == "information_schema" || d.name == "performance_schema" {
			t.Errorf("不可备份的系统库 %s 应被跳过", d.name)
		}
	}
}

func TestMysqlSingleDBErrors(t *testing.T) {
	// env 失败
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) { return nil, []byte("x"), 1 }
	dc := newFakeDockerClient(fake)
	if _, err := mysqlSingleDB(context.Background(), nil, dc, "c1"); err == nil {
		t.Error("env 失败应返回错误")
	}

	// auth 失败（无密码）
	fake = newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "env" {
			return []byte("NOT_A_PASSWORD=1\n"), nil, 0
		}
		return nil, nil, 0
	}
	dc = newFakeDockerClient(fake)
	if _, err := mysqlSingleDB(context.Background(), nil, dc, "c1"); err == nil {
		t.Error("缺少密码应返回错误")
	}

	// 列出数据库失败
	fake = newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "env" {
			return []byte("MYSQL_ROOT_PASSWORD=root\n"), nil, 0
		}
		return nil, []byte("x"), 1
	}
	dc = newFakeDockerClient(fake)
	if _, err := mysqlSingleDB(context.Background(), nil, dc, "c1"); err == nil {
		t.Error("列出数据库失败应返回错误")
	}
}

func TestValidateDBName(t *testing.T) {
	for _, good := range []string{"appdb", "my-db", "db_1", "db.v2"} {
		if err := validateDBName(good); err != nil {
			t.Errorf("合法库名 %q 被拒绝: %v", good, err)
		}
	}
	for _, bad := range []string{"../etc", "a/b", "", "db;rm"} {
		if err := validateDBName(bad); err == nil {
			t.Errorf("危险库名 %q 应被拒绝", bad)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("appdb"); got != "appdb" {
		t.Errorf("普通字符串不应加引号, got %q", got)
	}
	if got := shellQuote("my db"); got != "'my db'" {
		t.Errorf("含空格应加引号, got %q", got)
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("单引号应被转义, got %q", got)
	}
}

func TestGetBackupProviderByLabel(t *testing.T) {
	if p := getBackupProviderByLabel("postgres"); p == nil || p.name != "postgres" {
		t.Error("backup.provider=postgres 应匹配到 postgres provider")
	}
	if p := getBackupProviderByLabel(""); p != nil {
		t.Error("空标签不应匹配")
	}
	if p := getBackupProviderByLabel("unknown"); p != nil {
		t.Error("未知 provider 不应匹配")
	}
}
