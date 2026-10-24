package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// errImageUnavailable 模拟镜像信息不可读（ImageInspect 失败）。
var errImageUnavailable = errors.New("fake: image not found")

// sqliteFixtureBytes 构造一个含合法 SQLite 文件头的最小快照。
// 校验逻辑只依赖前 16 字节魔数，其余内容留白。
func sqliteFixtureBytes() []byte {
	buf := make([]byte, 200)
	copy(buf, "SQLite format 3\x00")
	return buf
}

func setupSQLiteFake(fake *fakeAPIClient, cid, image string) {
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{Image: image}}
	fake.imageTags[image] = []string{image}
}

// TestSqliteBackupCommandFromImageName 覆盖镜像名约定：felix2yu/shenshi → shenshi backup。
func TestSqliteBackupCommandFromImageName(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if len(cmd) == 1 && cmd[0] == "env" {
			return []byte("SHENSHI_DB=/data/shenshi.db\n"), nil, 0
		}
		return nil, nil, 0
	}
	cid := "cid-sqlite"
	setupSQLiteFake(fake, cid, "ghcr.io/felix2yu/shenshi:latest")
	dc := newFakeDockerClient(fake)

	cmd, env, err := sqliteBackupCommand(context.Background(), nil, dc, cid)
	if err != nil {
		t.Fatalf("sqliteBackupCommand: %v", err)
	}
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[2] != "shenshi backup" {
		t.Errorf("镜像名约定推导失败, got %v", cmd)
	}
	if len(env) != 0 {
		t.Errorf("不应注入额外环境变量, got %v", env)
	}
}

// TestSqliteBackupCommandEnvOverride 覆盖 BACKUP_SQLITE_CMD 的优先级。
func TestSqliteBackupCommandEnvOverride(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if len(cmd) == 1 && cmd[0] == "env" {
			return []byte("BACKUP_SQLITE_CMD=/app/bin/mydb -c /etc/db.toml backup\n"), nil, 0
		}
		return nil, nil, 0
	}
	cid := "cid-sqlite"
	setupSQLiteFake(fake, cid, "felix2yu/shenshi:latest")
	dc := newFakeDockerClient(fake)

	cmd, _, err := sqliteBackupCommand(context.Background(), nil, dc, cid)
	if err != nil {
		t.Fatalf("sqliteBackupCommand: %v", err)
	}
	joined := strings.Join(cmd, " ")
	if !strings.Contains(joined, "/app/bin/mydb -c /etc/db.toml backup") {
		t.Errorf("BACKUP_SQLITE_CMD 应原样生效, got %v", cmd)
	}
}

// TestSqliteBackupCommandEnvQuoted 覆盖引号剥壳：compose 列表式 KEY="value" 会把引号
// 存进值里，sh -c 整串被当命令名。契约值是最外层引号无语义，读取时剥掉。
func TestSqliteBackupCommandEnvQuoted(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"双引号包裹", `BACKUP_SQLITE_CMD="/app/mujian backup"`, "/app/mujian backup"},
		{"单引号包裹", `BACKUP_SQLITE_CMD='/app/mujian backup'`, "/app/mujian backup"},
		{"裸值不受影响", `BACKUP_SQLITE_CMD=/app/mujian backup`, "/app/mujian backup"},
		{"内部引号不剥", `BACKUP_SQLITE_CMD=/app/backup --note "hi"`, `/app/backup --note "hi"`},
		{"不成对引号不剥", `BACKUP_SQLITE_CMD="/app/mujian`, `"/app/mujian`},
	}
	for _, c := range cases {
		fake := newFakeAPIClient()
		fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
			if len(cmd) == 1 && cmd[0] == "env" {
				return []byte(c.env + "\n"), nil, 0
			}
			return nil, nil, 0
		}
		cid := "cid-quoted"
		setupSQLiteFake(fake, cid, "felix2yu/mujian:latest")
		dc := newFakeDockerClient(fake)

		cmd, _, err := sqliteBackupCommand(context.Background(), nil, dc, cid)
		if err != nil {
			t.Fatalf("%s: sqliteBackupCommand: %v", c.name, err)
		}
		if len(cmd) != 3 || cmd[2] != c.want {
			t.Errorf("%s: cmd = %v, want sh -c %q", c.name, cmd, c.want)
		}
	}
}

// TestSqliteBackupCommandEnvReadErrorFallsBack 覆盖 env 不可读时回退镜像名推导（而不是直接失败）。
func TestSqliteBackupCommandEnvReadErrorFallsBack(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if len(cmd) == 1 && cmd[0] == "env" {
			return nil, []byte("env: not found"), 127
		}
		return nil, nil, 0
	}
	cid := "cid-sqlite"
	setupSQLiteFake(fake, cid, "felix2yu/qingye:latest")
	dc := newFakeDockerClient(fake)

	cmd, _, err := sqliteBackupCommand(context.Background(), nil, dc, cid)
	if err != nil {
		t.Fatalf("env 不可读时应回退镜像名推导: %v", err)
	}
	if strings.Join(cmd, " ") != "sh -c qingye backup" {
		t.Errorf("回退命令异常, got %v", cmd)
	}
}

// TestSqliteBackupCommandNoBinary 覆盖镜像名不可用时的报错（提示改用 BACKUP_SQLITE_CMD）。
func TestSqliteBackupCommandNoBinary(t *testing.T) {
	fake := newFakeAPIClient()
	fake.imageInspectErr = errImageUnavailable
	cid := "cid-unknown"
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{Image: "sha256:deadbeef"}}
	dc := newFakeDockerClient(fake)

	_, _, err := sqliteBackupCommand(context.Background(), nil, dc, cid)
	if err == nil {
		t.Fatal("无法推导二进制名时应报错")
	}
	if !strings.Contains(err.Error(), envSQLiteBackupCmd) {
		t.Errorf("错误信息应提示 %s, got %v", envSQLiteBackupCmd, err)
	}
}

// TestSqliteBinaryNameRejectsUnsafe 覆盖注入防御：镜像名里的 shell 元字符不得进入命令。
func TestSqliteBinaryNameRejectsUnsafe(t *testing.T) {
	for _, name := range []string{"foo;rm -rf /", "a b", "$(id)", "..", ""} {
		if sqliteBinaryNamePattern.MatchString(name) {
			t.Errorf("危险二进制名 %q 不应通过校验", name)
		}
	}
	if !sqliteBinaryNamePattern.MatchString("shenshi") {
		t.Error("常规二进制名应通过校验")
	}
}

// TestProbeSQLiteContainer 覆盖声明与疑似两种探测结果。
func TestProbeSQLiteContainer(t *testing.T) {
	cases := map[string]struct {
		env          string
		wantDeclared bool
		wantLikely   bool
	}{
		"声明导出命令":         {"BACKUP_SQLITE_CMD=shenshi backup\n", true, true},
		"仅出现 db 路径":      {"DB_PATH=/data/app.db\n", false, true},
		"仅出现 sqlite DSN": {"DATABASE_URL=sqlite:///./data/web.db\n", false, true},
		"无关环境变量":         {"PATH=/usr/bin\nTZ=Asia/Shanghai\n", false, false},
	}
	for name, c := range cases {
		fake := newFakeAPIClient()
		fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
			return []byte(c.env), nil, 0
		}
		dc := newFakeDockerClient(fake)
		declared, likely := probeSQLiteContainer(context.Background(), dc, "c1")
		if declared != c.wantDeclared || likely != c.wantLikely {
			t.Errorf("%s: declared=%v likely=%v, want declared=%v likely=%v",
				name, declared, likely, c.wantDeclared, c.wantLikely)
		}
	}
}

// TestProbeSQLiteContainerEnvError 覆盖 env 读取失败时静默返回（不得中断扫描）。
func TestProbeSQLiteContainerEnvError(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		return nil, []byte("boom"), 1
	}
	dc := newFakeDockerClient(fake)
	declared, likely := probeSQLiteContainer(context.Background(), dc, "c1")
	if declared || likely {
		t.Errorf("env 读取失败时不应宣称命中, got declared=%v likely=%v", declared, likely)
	}
}

// TestBuildPlansSQLiteByLabel 覆盖 backup.provider=sqlite 标签识别。
func TestBuildPlansSQLiteByLabel(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) { return nil, nil, 0 }
	cid := "cid-sqlite"
	fake.inspect[cid] = container.InspectResponse{Config: &container.Config{
		Image:  "felix2yu/shenshi:latest",
		Labels: map[string]string{labelBackupProvider: "sqlite"},
	}}
	fake.imageTags["felix2yu/shenshi:latest"] = []string{"felix2yu/shenshi:latest"}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)

	plans, skipped, unmatched := buildPlans(context.Background(), cfg, dc,
		[]container.Summary{{ID: cid, Names: []string{"/shenshi"}}}, t.TempDir())
	if len(plans) != 1 || skipped != 0 || len(unmatched) != 0 {
		t.Fatalf("plans=%d skipped=%d unmatched=%v", len(plans), skipped, unmatched)
	}
	if plans[0].provider.name != sqliteProviderName {
		t.Errorf("provider = %q, want sqlite", plans[0].provider.name)
	}
	if got := strings.Join(plans[0].command, " "); got != "sh -c shenshi backup" {
		t.Errorf("command = %q, want \"sh -c shenshi backup\"", got)
	}
}

// TestBuildPlansSQLiteByEnv 覆盖 BACKUP_SQLITE_CMD 自动识别（无需标签）。
func TestBuildPlansSQLiteByEnv(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if len(cmd) == 1 && cmd[0] == "env" {
			return []byte("BACKUP_SQLITE_CMD=qingye backup\nDB_PATH=/app/data/qingye.db\n"), nil, 0
		}
		return nil, nil, 0
	}
	cid := "cid-qingye"
	setupSQLiteFake(fake, cid, "ghcr.io/felix2yu/qingye:latest")
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)

	plans, skipped, _ := buildPlans(context.Background(), cfg, dc,
		[]container.Summary{{ID: cid, Names: []string{"/qingye"}}}, t.TempDir())
	if len(plans) != 1 || skipped != 0 {
		t.Fatalf("plans=%d skipped=%d, want 1/0", len(plans), skipped)
	}
	if got := strings.Join(plans[0].command, " "); got != "sh -c qingye backup" {
		t.Errorf("command = %q, want \"sh -c qingye backup\"", got)
	}
}

// TestBuildPlansSQLiteLikelyUnmatched 覆盖防漏备份：出现 .db 线索但未声明导出命令时上报疑似。
func TestBuildPlansSQLiteLikelyUnmatched(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		if len(cmd) == 1 && cmd[0] == "env" {
			return []byte("DB_PATH=/data/app.db\n"), nil, 0
		}
		return nil, nil, 0
	}
	cid := "cid-app"
	setupSQLiteFake(fake, cid, "felix2yu/mujian:latest")
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)

	plans, skipped, unmatched := buildPlans(context.Background(), cfg, dc,
		[]container.Summary{{ID: cid, Names: []string{"/mujian"}}}, t.TempDir())
	if len(plans) != 0 || skipped != 1 {
		t.Fatalf("plans=%d skipped=%d, want 0/1", len(plans), skipped)
	}
	if len(unmatched) != 1 || unmatched[0] != "mujian" {
		t.Errorf("疑似 SQLite 容器应计入 unmatched, got %v", unmatched)
	}
}

// TestBackupContainerSQLiteEndToEnd 覆盖完整链路：识别 → exec 应用自导出 → 魔数校验 → 落盘 → 清单。
func TestBackupContainerSQLiteEndToEnd(t *testing.T) {
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		switch {
		case len(cmd) == 1 && cmd[0] == "env":
			return []byte("BACKUP_SQLITE_CMD=shenshi backup\nSHENSHI_DB=/data/shenshi.db\n"), nil, 0
		case strings.Contains(strings.Join(cmd, " "), "shenshi backup"):
			return sqliteFixtureBytes(), nil, 0
		}
		return nil, nil, 0
	}
	cid := "cid-shenshi"
	setupSQLiteFake(fake, cid, "ghcr.io/felix2yu/shenshi:latest")
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/shenshi"}}}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)

	at := time.Now()
	dir := runBackupOnce(t, cfg, dc, at)

	matches, _ := filepath.Glob(filepath.Join(dir, "shenshi.db*"))
	if len(matches) != 1 {
		t.Fatalf("应生成 shenshi.db 备份文件, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("读取备份文件失败: %v", err)
	}
	if !strings.HasPrefix(string(data), "SQLite format 3\x00") {
		t.Error("备份文件应保留 SQLite 魔数")
	}

	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("应写入备份清单: %v", err)
	}
	if m.Status != statusSuccess || len(m.Containers) != 1 {
		t.Fatalf("清单异常: status=%s containers=%d", m.Status, len(m.Containers))
	}
	if m.Containers[0].Provider != sqliteProviderName {
		t.Errorf("provider = %q, want sqlite", m.Containers[0].Provider)
	}
	if m.Containers[0].Mode != modeFull {
		t.Errorf("mode = %q, want full", m.Containers[0].Mode)
	}
	if len(m.Containers[0].Files) != 1 || m.Containers[0].Files[0].Size == 0 {
		t.Errorf("清单应记录备份文件: %+v", m.Containers[0].Files)
	}
	if !strings.HasSuffix(m.Containers[0].Files[0].Path, "shenshi.db") {
		t.Errorf("文件路径异常: %s", m.Containers[0].Files[0].Path)
	}
}

// TestSqliteFileExtOverride 覆盖 BACKUP_SQLITE_EXT：合法值覆写扩展名，非法值被忽略。
func TestSqliteFileExtOverride(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"BACKUP_SQLITE_EXT=zip\n", "zip"},
		{"BACKUP_SQLITE_EXT=ZIP\n", "zip"},
		{"BACKUP_SQLITE_EXT=\"zip\"\n", "zip"},
		{"BACKUP_SQLITE_EXT='zip'\n", "zip"},
		{"", "db"},
		{"BACKUP_SQLITE_EXT=../evil\n", "db"},
		{"BACKUP_SQLITE_EXT=db.bak\n", "db"},
	}
	for _, c := range cases {
		fake := newFakeAPIClient()
		fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
			return []byte("BACKUP_SQLITE_CMD=app backup\n" + c.env), nil, 0
		}
		cid := "cid-app"
		setupSQLiteFake(fake, cid, "felix2yu/app:latest")
		dc := newFakeDockerClient(fake)
		cfg := testBackupConfig(t)

		plans, _, _ := buildPlans(context.Background(), cfg, dc,
			[]container.Summary{{ID: cid, Names: []string{"/app"}}}, t.TempDir())
		if len(plans) != 1 {
			t.Fatalf("env %q: 应识别为 sqlite 容器", c.env)
		}
		if plans[0].fileExt != c.want {
			t.Errorf("env %q: fileExt = %q, want %q", c.env, plans[0].fileExt, c.want)
		}

		tasks := collectTasks(cfg, plans)
		if !strings.HasSuffix(tasks[0].filePath, "."+c.want) {
			t.Errorf("env %q: 产物文件名 = %q, 应以 .%s 结尾", c.env, tasks[0].filePath, c.want)
		}
	}
}

// TestSqliteExecUser 覆盖 exec 身份解析：显式声明 > PUID/PGID > 默认（空）。
func TestSqliteExecUser(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"", ""},
		{"PUID=1000\nPGID=1000\n", "1000:1000"},
		{"PUID=1000\n", ""},
		{"PUID=abc\nPGID=1000\n", ""},
		{"BACKUP_SQLITE_USER=1001:1002\nPUID=1000\nPGID=1000\n", "1001:1002"},
		{"BACKUP_SQLITE_USER=\"1001:1002\"\n", "1001:1002"},
		{"BACKUP_SQLITE_USER='root'\n", "root"},
		{"PUID=\"1000\"\nPGID=\"1000\"\n", "1000:1000"},
		{"BACKUP_SQLITE_USER=root\n", "root"},
	}
	for _, c := range cases {
		fake := newFakeAPIClient()
		fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
			return []byte(c.env), nil, 0
		}
		dc := newFakeDockerClient(fake)
		if got := sqliteExecUser(context.Background(), dc, "c1"); got != c.want {
			t.Errorf("env %q: execUser = %q, want %q", c.env, got, c.want)
		}
	}
}

// TestBackupContainerSQLiteZipEndToEnd 覆盖 zip 产物链路：EXT 覆写 → exec 身份 → 落盘 → zip 校验。
func TestBackupContainerSQLiteZipEndToEnd(t *testing.T) {
	zipBytes := buildTestZip(t)
	fake := newFakeAPIClient()
	fake.execHandler = func(cmd []string) ([]byte, []byte, int) {
		switch {
		case len(cmd) == 1 && cmd[0] == "env":
			return []byte("BACKUP_SQLITE_CMD=shenshi backup\nBACKUP_SQLITE_EXT=zip\nPUID=1000\nPGID=1000\n"), nil, 0
		case strings.Contains(strings.Join(cmd, " "), "shenshi backup"):
			return zipBytes, nil, 0
		}
		return nil, nil, 0
	}
	cid := "cid-shenshi"
	setupSQLiteFake(fake, cid, "ghcr.io/felix2yu/shenshi:latest")
	fake.containers = []container.Summary{{ID: cid, Names: []string{"/shenshi"}}}
	dc := newFakeDockerClient(fake)
	cfg := testBackupConfig(t)

	at := time.Now()
	dir := runBackupOnce(t, cfg, dc, at)

	if _, err := os.Stat(filepath.Join(dir, "shenshi.zip")); err != nil {
		t.Fatalf("应生成 shenshi.zip: %v", err)
	}
	// 无法推导时的兜底行为不受影响：执行用户应来自 PUID/PGID。
	if fake.lastExecUser != "1000:1000" {
		t.Errorf("exec user = %q, want 1000:1000", fake.lastExecUser)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("应写入备份清单: %v", err)
	}
	if m.Status != statusSuccess || len(m.Containers) != 1 {
		t.Fatalf("清单异常: status=%s containers=%d", m.Status, len(m.Containers))
	}
	if got := m.Containers[0].Files[0].Path; !strings.HasSuffix(got, "shenshi.zip") {
		t.Errorf("清单文件路径异常: %s", got)
	}
}

// TestValidateBackupZIP 覆盖 zip 扩展名的魔数校验。
func TestValidateBackupZIP(t *testing.T) {
	if err := validateBackupContent(bytes.NewReader(buildTestZip(t)), "zip"); err != nil {
		t.Errorf("合法 zip 备份被拒绝: %v", err)
	}
	// 空压缩包（只有中央目录）对备份没有意义，应被拒绝。
	empty := []byte{'P', 'K', 0x05, 0x06, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := validateBackupContent(bytes.NewReader(empty), "zip"); err == nil {
		t.Error("空 zip 应被拒绝")
	}
	if err := validateBackupContent(strings.NewReader("不是压缩包的内容"), "zip"); err == nil {
		t.Error("非 zip 内容应被拒绝")
	}
}

// buildTestZip 构造一个最小的合法 zip（内含一个条目），供魔数校验测试使用。
func buildTestZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("shenshi-backup.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestValidateBackupSQLiteMagic 覆盖 db 扩展名的魔数校验。
func TestValidateBackupSQLiteMagic(t *testing.T) {
	if err := validateBackupContent(strings.NewReader(string(sqliteFixtureBytes())), "db"); err != nil {
		t.Errorf("合法 SQLite 快照被拒绝: %v", err)
	}
	// 截断到 10 字节：连魔数都不完整
	if err := validateBackupContent(strings.NewReader("SQLite for"), "db"); err == nil {
		t.Error("魔数不完整应被拒绝")
	}
	if err := validateBackupContent(strings.NewReader("<html>error</html>"), "db"); err == nil {
		t.Error("非 SQLite 内容应被拒绝")
	}
}

// TestValidatePlainBackupSQLite 覆盖 plain 落盘路径（只读首尾）下的 SQLite 校验。
func TestValidatePlainBackupSQLite(t *testing.T) {
	dir := t.TempDir()
	cfg := &config{compression: "plain"}

	path := filepath.Join(dir, "app.db")
	if err := os.WriteFile(path, sqliteFixtureBytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupFile(cfg, path, "db"); err != nil {
		t.Errorf("合法快照校验失败: %v", err)
	}

	if err := os.WriteFile(path, []byte("GARBAGE-CONTENT-0000000000000000000000"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupFile(cfg, path, "db"); err == nil {
		t.Error("非法内容应校验失败")
	}
}
