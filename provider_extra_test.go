package main

import (
	"context"
	"strings"
	"testing"
)

func TestPostgresUserEnv(t *testing.T) {
	if got := postgresUserEnv(map[string]string{}); got != "postgres" {
		t.Errorf("默认应为 postgres, got %q", got)
	}
	if got := postgresUserEnv(map[string]string{"POSTGRES_USER": "alice"}); got != "alice" {
		t.Errorf("应为 alice, got %q", got)
	}
}

func TestMysqlAuth(t *testing.T) {
	got, err := mysqlAuth(map[string]string{"MARIADB_ROOT_PASSWORD": "p1"})
	if err != nil || got != "-p$MARIADB_ROOT_PASSWORD" {
		t.Errorf("got %q err %v, want -p$MARIADB_ROOT_PASSWORD", got, err)
	}
	got, err = mysqlAuth(map[string]string{"MYSQL_ROOT_PASSWORD": "p2"})
	if err != nil || got != "-p$MYSQL_ROOT_PASSWORD" {
		t.Errorf("got %q err %v, want -p$MYSQL_ROOT_PASSWORD", got, err)
	}
	if _, err := mysqlAuth(map[string]string{}); err == nil {
		t.Error("缺少密码时应返回错误")
	}
}

func TestPsqlBackupCommand(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	cmd, err := psqlBackupCommand(context.Background(), dc, "c1")
	if err != nil {
		t.Fatalf("psqlBackupCommand: %v", err)
	}
	if len(cmd) != 3 || cmd[0] != "pg_dumpall" || cmd[2] != "postgres" {
		t.Errorf("unexpected cmd: %v", cmd)
	}
}

func TestMysqlBackupCommand(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	cmd, err := mysqlBackupCommand(context.Background(), dc, "c1")
	if err != nil {
		t.Fatalf("mysqlBackupCommand: %v", err)
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "mysqldump") || !strings.Contains(joined, "-p$MYSQL_ROOT_PASSWORD") {
		t.Errorf("unexpected cmd: %v", cmd)
	}
	if !strings.Contains(joined, "--all-databases") {
		t.Errorf("mysql 备份应包含所有数据库: %v", cmd)
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
	cmd, err := mysqlBackupCommand(context.Background(), dc, "c1")
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
	cmd, err := redisBackupCommand(context.Background(), dc, "c1")
	if err != nil {
		t.Fatalf("redisBackupCommand: %v", err)
	}
	if !strings.Contains(strings.Join(cmd, " "), "redis-cli SAVE") {
		t.Errorf("unexpected cmd: %v", cmd)
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
	cmd, err := redisBackupCommand(context.Background(), dc, "c1")
	if err != nil {
		t.Fatalf("redisBackupCommand: %v", err)
	}
	if !strings.Contains(strings.Join(cmd, " "), "valkey-cli SAVE") {
		t.Errorf("存在 valkey-cli 时应优先使用: %v", cmd)
	}
}

func TestPsqlSingleDB(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	dbs, err := psqlSingleDB(context.Background(), dc, "c1")
	if err != nil {
		t.Fatalf("psqlSingleDB: %v", err)
	}
	var names []string
	for _, d := range dbs {
		names = append(names, d.name)
		if d.name == "globals" && !d.isSystem {
			t.Error("globals 应标记为系统库")
		}
	}
	if len(dbs) == 0 {
		t.Fatal("应列出数据库")
	}
	_ = names
}

func TestPsqlSingleDBEnvError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		return nil, []byte("err"), 1
	}
	dc := newFakeDockerClient(fake)
	if _, err := psqlSingleDB(context.Background(), dc, "c1"); err == nil {
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
	if _, err := psqlSingleDB(context.Background(), dc, "c1"); err == nil {
		t.Error("列出数据库失败应返回错误")
	}
}

func TestMysqlSingleDB(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = dumpHandler
	dc := newFakeDockerClient(fake)
	dbs, err := mysqlSingleDB(context.Background(), dc, "c1")
	if err != nil {
		t.Fatalf("mysqlSingleDB: %v", err)
	}
	for _, d := range dbs {
		if d.name == "mysql" {
			t.Error("系统库 mysql 不应出现在单库列表中")
		}
	}
	if len(dbs) == 0 {
		t.Fatal("应列出非系统数据库")
	}
}

func TestMysqlSingleDBErrors(t *testing.T) {
	// env 失败
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) { return nil, []byte("x"), 1 }
	dc := newFakeDockerClient(fake)
	if _, err := mysqlSingleDB(context.Background(), dc, "c1"); err == nil {
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
	if _, err := mysqlSingleDB(context.Background(), dc, "c1"); err == nil {
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
	if _, err := mysqlSingleDB(context.Background(), dc, "c1"); err == nil {
		t.Error("列出数据库失败应返回错误")
	}
}
