package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// redisScriptStub 是一个假的 redis-cli，只实现备份脚本用到的子命令：
// BGSAVE 成功时把状态文件置位，之后的 INFO persistence 就会报告新的 rdb_last_save_time。
// 借此可以在本地真实执行生成的备份脚本，验证"BGSAVE 没落盘时绝不能 cat 出旧 RDB"。
const redisScriptStub = `#!/bin/sh
case "$*" in
  BGSAVE)
    if [ "$STUB_BGSAVE_WORKS" = "yes" ]; then echo saved > "$STUB_DIR/state"; fi
    exit 0
    ;;
  *"INFO persistence")
    st=$STUB_SAVE_TIME
    changes=$STUB_CHANGES
    if [ -f "$STUB_DIR/state" ]; then
      st=$((STUB_SAVE_TIME + 1))
      changes=0
    fi
    printf 'rdb_bgsave_in_progress:0\r\n'
    printf 'rdb_last_bgsave_status:%s\r\n' "$STUB_STATUS"
    printf 'rdb_last_save_time:%s\r\n' "$st"
    printf 'rdb_changes_since_last_save:%s\r\n' "$changes"
    exit 0
    ;;
esac
exit 0
`

func runGeneratedRedisScript(t *testing.T, bgsaveWorks, status, changes string) (stdout, stderr string, err error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("需要 POSIX sh")
	}

	root := t.TempDir()
	rdbDir := filepath.Join(root, "data")
	if err := os.MkdirAll(rdbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "REDIS0011" + strings.Repeat("x", 32) + "\xff"
	if err := os.WriteFile(filepath.Join(rdbDir, "dump.rdb"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	stubDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "redis-cli"), []byte(redisScriptStub), 0o755); err != nil {
		t.Fatal(err)
	}

	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		joined := strings.Join(cmd, " ")
		switch {
		case cmd[0] == "which":
			return nil, nil, 1
		case strings.Contains(joined, "CONFIG GET dir"):
			return []byte("dir\n" + rdbDir + "\n"), nil, 0
		case strings.Contains(joined, "CONFIG GET dbfilename"):
			return []byte("dbfilename\ndump.rdb\n"), nil, 0
		}
		return nil, nil, 0
	}
	dc := newFakeDockerClient(fake)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd, _, err := redisBackupCommand(ctx, nil, dc, "c1")
	if err != nil {
		t.Fatalf("redisBackupCommand: %v", err)
	}

	var out, errBuf strings.Builder
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdout, c.Stderr = &out, &errBuf
	c.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_DIR="+root,
		"STUB_BGSAVE_WORKS="+bgsaveWorks,
		"STUB_STATUS="+status,
		"STUB_CHANGES="+changes,
		"STUB_SAVE_TIME=1000",
	)
	runErr := c.Run()
	return out.String(), errBuf.String(), runErr
}

// 成功路径：BGSAVE 真的落了盘，才交出 RDB 内容。
func TestRedisScriptDumpsOnSuccessfulBGSAVE(t *testing.T) {
	stdout, stderr, err := runGeneratedRedisScript(t, "yes", "ok", "42")
	if err != nil {
		t.Fatalf("脚本应成功: %v, stderr=%s", err, stderr)
	}
	if !strings.HasPrefix(stdout, "REDIS0011") {
		t.Errorf("应输出 RDB 内容, got %q", stdout)
	}
}

// C3 回归：BGSAVE 没产生新 RDB（磁盘满 / fork 失败）时，标志位同样会归零，
// 旧实现会直接 cat 出上一次保存的文件并判成功。
func TestRedisScriptRejectsStaleRDB(t *testing.T) {
	stdout, stderr, err := runGeneratedRedisScript(t, "no", "ok", "42")
	if err == nil {
		t.Fatalf("陈旧 RDB 必须判失败, stdout=%q", stdout)
	}
	if strings.HasPrefix(stdout, "REDIS0011") {
		t.Error("拒绝陈旧备份时不得输出 RDB 内容")
	}
	if !strings.Contains(stderr, "未产生新的 RDB") {
		t.Errorf("应说明拒绝原因, got %q", stderr)
	}
}

// rdb_last_bgsave_status:err 单独也是一道闸门，不依赖时间戳是否前进。
func TestRedisScriptRejectsBgsaveErrorStatus(t *testing.T) {
	stdout, stderr, err := runGeneratedRedisScript(t, "no", "err", "42")
	if err == nil {
		t.Fatalf("err 状态必须判失败, stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "rdb_last_bgsave_status:err") {
		t.Errorf("应上报 err 状态, got %q", stderr)
	}
}

// 覆盖 provider 侧的命令拼装：脚本必须落到动态获取的 RDB 路径上。
func TestRedisScriptTargetsConfiguredPath(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		joined := strings.Join(cmd, " ")
		switch {
		case cmd[0] == "which":
			return nil, nil, 1
		case strings.Contains(joined, "CONFIG GET dir"):
			return []byte("dir\n/var/lib/redis\n"), nil, 0
		case strings.Contains(joined, "CONFIG GET dbfilename"):
			return []byte("dbfilename\nsnap.rdb\n"), nil, 0
		}
		return nil, nil, 0
	}
	dc := newFakeDockerClient(fake)
	cmd, _, err := redisBackupCommand(context.Background(), nil, dc, "c1")
	if err != nil {
		t.Fatalf("redisBackupCommand: %v", err)
	}
	if !strings.Contains(cmd[2], "cat /var/lib/redis/snap.rdb") {
		t.Errorf("应 cat 配置中的 RDB 路径, got %q", cmd[2])
	}
	if !strings.Contains(cmd[2], "rdb_last_save_time") {
		t.Errorf("脚本应包含新 RDB 证据检查, got %q", cmd[2])
	}
}
