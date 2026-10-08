package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

// 校验函数本身也要能报告"文件读不出来"，而不是把损坏当成合法备份。
func TestValidateBackupFileIOErrors(t *testing.T) {
	dir := t.TempDir()
	plain := &config{compression: "plain"}

	if err := validateBackupFile(plain, filepath.Join(dir, "missing.sql"), "sql"); err == nil {
		t.Error("文件不存在时应报错")
	}

	// 压缩算法与实际内容不符：解压器创建失败，不能退化成"跳过校验"
	content := filepath.Join(dir, "appdb.sql")
	if err := os.WriteFile(content, []byte(validSQLDump), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupFile(&config{compression: "gzip"}, content, "sql"); err == nil {
		t.Error("plain 文件按 gzip 校验应报错")
	}
	if err := validateBackupFile(&config{compression: "bz2"}, content, "sql"); err == nil {
		t.Error("plain 文件按 bz2 校验应报错")
	}

	// 空文件
	empty := filepath.Join(dir, "empty.sql")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupFile(plain, empty, "sql"); err == nil || !strings.Contains(err.Error(), "备份为空") {
		t.Errorf("空文件应报备份为空, got %v", err)
	}

	// 目录无法按文件读取
	if err := validateBackupFile(plain, dir, "sql"); err == nil {
		t.Error("对目录做校验应报错")
	}
}

func TestValidateBackupFileGzipRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "appdb.sql.gz")

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(validSQLDump)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := &config{compression: "gzip"}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupFile(cfg, path, "sql"); err != nil {
		t.Errorf("完好 gzip 应通过: %v", err)
	}

	// 截断的 gzip：解压中途失败必须被检出
	truncated := filepath.Join(dir, "cut.sql.gz")
	data := buf.Bytes()
	if err := os.WriteFile(truncated, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupFile(cfg, truncated, "sql"); err == nil {
		t.Error("截断的 gzip 应报错")
	}
}

type chunkReader struct {
	data  []byte
	at    int
	chunk int
	err   error
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.at >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(r.data)-r.at {
		n = len(r.data) - r.at
	}
	copy(p, r.data[r.at:r.at+n])
	r.at += n
	return n, nil
}

func TestValidateBackupContentBuffering(t *testing.T) {
	// 小于首缓冲区的输入：走"整块并入 head"的分支
	body := []byte("-- MySQL dump\n-- Dump completed on 2026-09-21\n")
	if err := validateBackupContent(&chunkReader{data: body, chunk: 7}, "sql"); err != nil {
		t.Errorf("小输入应通过结构校验: %v", err)
	}

	// 超过尾缓冲区：必须滚动丢弃旧数据，只保留尾部
	var big bytes.Buffer
	big.WriteString(strings.Repeat("x", 200<<10))
	big.WriteString("-- Dump completed on 2026-09-21\n")
	if err := validateBackupContent(&chunkReader{data: big.Bytes(), chunk: 64 << 10}, "sql"); err != nil {
		t.Errorf("大输入应能通过（尾部标记保留正确）: %v", err)
	}

	if err := validateBackupContent(strings.NewReader(""), "sql"); err == nil ||
		!strings.Contains(err.Error(), "备份为空") {
		t.Errorf("零字节内容应报备份为空, got %v", err)
	}

	readErr := errors.New("读盘失败")
	if err := validateBackupContent(&chunkReader{err: readErr}, "sql"); err == nil ||
		!strings.Contains(err.Error(), "读取备份内容失败") {
		t.Errorf("读取失败应上报, got %v", err)
	}
}

func TestCheckBackupStructureDirectly(t *testing.T) {
	if err := checkBackupStructure([]byte("x"), []byte("x"), 0, "sql"); err == nil ||
		!strings.Contains(err.Error(), "备份为空") {
		t.Errorf("got %v", err)
	}
	if err := checkBackupStructure([]byte("RED"), []byte("RED"), 3, "rdb"); err == nil ||
		!strings.Contains(err.Error(), "过短") {
		t.Errorf("RDB 头过短应报错, got %v", err)
	}
	if err := checkBackupStructure([]byte("VALKE0011"), []byte("no marker"), 9, "rdb"); err == nil ||
		!strings.Contains(err.Error(), "结束标记") {
		t.Errorf("缺少结束标记应报错, got %v", err)
	}
	// valkey 的魔数同样合法，尾字节直接是 0xFF（关闭校验和时）
	if err := checkBackupStructure([]byte("VALKE0011"), []byte{'\xff'}, 9, "rdb"); err != nil {
		t.Errorf("VALKE 魔数应被接受: %v", err)
	}
	// 开启 rdbchecksum 时 0xFF 后面还跟 8 字节 CRC64，即 0xFF 位于倒数第 9 字节
	if err := checkBackupStructure([]byte("REDIS0011"), []byte("body\xff12345678"), 17, "rdb"); err != nil {
		t.Errorf("带 CRC64 的真实布局应被接受: %v", err)
	}
	if rdbHasEOFMarker(nil) {
		t.Error("空尾部不应含结束标记")
	}
	if err := checkBackupStructure([]byte("x"), []byte("x"), 1, ""); err != nil {
		t.Errorf("未知扩展名不应误报: %v", err)
	}
}

func TestBackupCommandCredentialErrors(t *testing.T) {
	ctx := context.Background()

	failing := newFakeDockerClient(&fakeAPIClient{
		inspect: map[string]container.InspectResponse{}, imageTags: map[string][]string{},
		execs: map[string]*execResult{}, execCreateErr: errors.New("exec 不可用"),
	})
	if _, _, err := psqlBackupCommand(ctx, nil, failing, "c1"); err == nil {
		t.Error("读取容器环境失败时应返回错误")
	}
	if _, _, err := mysqlBackupCommand(ctx, nil, failing, "c1"); err == nil {
		t.Error("读取容器环境失败时应返回错误")
	}

	// 没有任何密码来源时，MySQL 必须直接失败而不是等交互输入
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "env" {
			return []byte("PATH=/usr/bin\n"), nil, 0
		}
		return nil, nil, 0
	}
	dc := newFakeDockerClient(fake)
	if _, _, err := mysqlBackupCommand(ctx, nil, dc, "c1"); err == nil {
		t.Error("缺凭据时应报错")
	}
}

func TestMysqlSingleDBPrefersMariadbDump(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		joined := strings.Join(cmd, " ")
		switch {
		case cmd[0] == "env":
			return []byte("MARIADB_ROOT_PASSWORD=root\n"), nil, 0
		case cmd[0] == "which":
			return []byte("/usr/bin/mariadb-dump\n"), nil, 0
		case strings.Contains(joined, "SCHEMA_NAME"):
			return []byte("appdb\n\nmysql\n"), nil, 0
		}
		return nil, nil, 0
	}
	dbs, err := mysqlSingleDB(context.Background(), nil, newFakeDockerClient(fake), "c1")
	if err != nil {
		t.Fatal(err)
	}
	// 输出里的空行应被忽略，而不是生成一个空库名的任务
	for _, d := range dbs {
		if d.name == "" {
			t.Fatal("空行不应生成备份任务")
		}
		if !strings.Contains(strings.Join(d.command, " "), "mariadb-dump") {
			t.Errorf("存在 mariadb-dump 时应优先使用: %v", d.command)
		}
	}
	if len(dbs) != 2 {
		t.Errorf("应跳过空行只留 2 个库, got %d: %+v", len(dbs), dbs)
	}
}

func TestPostgresUserAndEnvPrecedence(t *testing.T) {
	env := map[string]string{"POSTGRES_USER": "app", "POSTGRES_PASSWORD": "pgpass", "PGUSER": "pguser"}

	// 显式指定备份账号时不再注入 POSTGRES_PASSWORD（认证方式由 DBA 决定）
	if got := postgresExecEnv(&config{pgUser: "backup"}, env); got != nil {
		t.Errorf("got %v", got)
	}
	if got := postgresExecEnv(nil, env); len(got) != 1 || got[0] != "PGPASSWORD=pgpass" {
		t.Errorf("应优先用 POSTGRES_PASSWORD, got %v", got)
	}
	if got := postgresExecEnv(nil, map[string]string{"PGPASSWORD": "other"}); len(got) != 1 {
		t.Errorf("PGPASSWORD 也应被采用, got %v", got)
	}
	if got := postgresExecEnv(nil, map[string]string{}); got != nil {
		t.Errorf("无密码来源时应返回空, got %v", got)
	}

	if got := postgresUser(nil, env); got != "app" {
		t.Errorf("got %q", got)
	}
	if got := postgresUser(nil, map[string]string{"PGUSER": "x"}); got != "x" {
		t.Errorf("got %q", got)
	}
	if got := postgresUser(nil, map[string]string{}); got != "postgres" {
		t.Errorf("got %q", got)
	}

	// MySQL 用户名优先级：显式配置 > MYSQL_USER > root
	if user, _, err := mysqlCredentials(&config{mysqlUser: "backup", mysqlPassword: "pw"}, env); err != nil || user != "backup" {
		t.Errorf("got %q %v", user, err)
	}
	if user, _, err := mysqlCredentials(nil, map[string]string{"MYSQL_USER": "app", "MYSQL_PASSWORD": "p"}); err != nil || user != "app" {
		t.Errorf("应回退到 MYSQL_USER, got %q %v", user, err)
	}
	if _, _, err := mysqlCredentials(&config{mysqlUser: "backup"}, env); err == nil {
		t.Error("只给账号不给密码时应报错")
	}
}

func TestShellQuoteAndDBNameValidation(t *testing.T) {
	if got := shellQuote(""); got != "''" {
		t.Errorf("空串应引起来, got %q", got)
	}
	for _, safe := range []string{"appdb", "a-b.c_1", "MySQL8"} {
		if got := shellQuote(safe); got != safe {
			t.Errorf("普通名字不应加引号, got %q", got)
		}
	}
	for _, risky := range []string{"a b", "a'b", "a$b", "a;b", "a`b", "a*b", "a|b"} {
		got := shellQuote(risky)
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("含特殊字符的 %q 应被单引号包裹, got %q", risky, got)
		}
	}

	for _, bad := range []string{"", "../x", "a/b", `a\b`, ".", "..", "带中文"} {
		if err := validateDBName(bad); err == nil {
			t.Errorf("validateDBName(%q) 应拒绝", bad)
		}
	}
	for _, ok := range []string{"app", "app_db", "app-db", "app.db", "1"} {
		if err := validateDBName(ok); err != nil {
			t.Errorf("validateDBName(%q) 不应拒绝: %v", ok, err)
		}
	}
}

func TestRedisRDBPathFallsBackWhenConfigUnavailable(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if cmd[0] == "which" {
			return nil, nil, 1
		}
		return nil, []byte("ERR unknown command"), 1
	}
	cmd, _, err := redisBackupCommand(context.Background(), nil, newFakeDockerClient(fake), "c1")
	if err != nil {
		t.Fatal(err)
	}
	// CONFIG 不可用（受保护实例）时必须退回默认路径，而不是拼出空路径
	if !strings.Contains(cmd[2], "cat /data/dump.rdb") {
		t.Errorf("应回退到 /data/dump.rdb, got %q", cmd[2])
	}

	if got := parseConfigGet([]byte("dir\n")); got != "" {
		t.Errorf("单行输出应视为不可用, got %q", got)
	}
	if got := parseConfigGet([]byte("dir\n /var/lib/redis \n")); got != "/var/lib/redis" {
		t.Errorf("应去掉首尾空白, got %q", got)
	}
}
