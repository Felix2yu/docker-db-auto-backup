# SQLite 备份支持 —— 详细设计

## 1. 目标与范围

为 `docker-db-auto-backup` 增加 SQLite 备份能力，覆盖 Home Assistant、Vaultwarden、navidrome、FreshRSS、PhotoPrism 这类「应用容器内嵌 SQLite 文件」的场景。

**默认导出格式**：SQL 文本（`.dump`），即 `fileExt = "sql"`，与现有 postgres / mysql 行为对齐。

**非目标**：

- 不做宿主机上的 SQLite 备份（本项目只处理容器）。
- 不引入 Go 的 SQLite 驱动。`mattn/go-sqlite3` 需要 cgo，会破坏当前静态编译与镜像体积，且容器内已有官方 CLI 可用。
- 不接入恢复演练（理由见 §6）。

---

## 2. 现状：可复用的管道

`backupProvider` 只需提供一条容器内命令，其余全部由既有代码承担：

```
buildPlans → provider.singleDB / provider.backupMethod
  → collectTasks（展开为最小调度单元）
  → runTaskWithRetry（并发 + 超时 + 重试）
  → writeBackup（stdout 流式 → 临时文件 → 压缩 → 校验 → 原子 rename）
  → manifest / 异常检测 / Kopia / 恢复演练 / 保留清理 / 通知 / 心跳
```

`redisBackupCommand` 已经是 `sh -c "... cat dump.rdb"` 的先例，证明**二进制流**走这条管道同样可行。因此本次改动**不触碰 `backup.go` 的落盘逻辑**，只做增量扩展。

`backupProvider` 现结构（`provider.go:18`）：

```go
type backupProvider struct {
	name         string
	patterns     []string
	fileExt      string
	backupMethod func(ctx, cfg, dc, containerID) ([]string, []string, error)
	singleDB     func(ctx, cfg, dc, containerID) ([]database, error)
}
```

---

## 3. 三个必须正视的差异

### 3.1 无法靠镜像名识别

SQLite 不是服务型数据库，不存在 `sqlite:latest` 这类官方镜像；使用者是各类应用容器。`patterns` 匹配在此无意义，必须依赖已有的标签机制。

`getBackupProviderByLabel` 优先级高于镜像名匹配，且直接查 `providers` 数组，因此新增名为 `sqlite` 的 provider 后，`backup.provider=sqlite` 即自动生效，**无需改动识别逻辑**。

### 3.2 容器内通常没有 sqlite3 CLI

上述应用镜像基本不自带 `sqlite3` 二进制。可用现有的 `dc.hasBinary`（带 per-container 缓存）探测，据此选择策略。

### 3.3 一致性是真正的风险

直接拷贝一个正在被写入的 `.db` 文件，在 WAL 模式下会遗漏尚未 checkpoint 的 `-wal` 内容，恢复后极可能得到损坏或过期数据。

更进一步：`.dump` 是**逐表顺序读取**的。在 autocommit 模式下，跨表之间没有单一快照保证——表 A 在 T1 读取、表 B 在 T2 读取，若中间发生写入，得到的 dump 内部自相矛盾（外键、触发器、跨表业务逻辑都可能失效）。

**这是本设计的核心结论：不能直接用 `.dump` 打源库。**

---

## 4. 设计决策

### 决策 1：先 `.backup` 到容器内临时文件，再 `.dump` 该快照

组合使用两条命令，同时拿到「原子一致性」与「SQL 文本输出」：

```sh
set -e
t=/tmp/auto-backup-sqlite-<slug>.db
rm -f "$t"
if sqlite3 '<src>' ".backup '$t'" 2>/dev/null; then
  sqlite3 "$t" .dump || { rm -f "$t"; exit 1; }
else
  sqlite3 '<src>' .dump || exit 1
fi
printf '%s\n' '-- SQLite dump complete'
rm -f "$t"
```

要点：

- `.backup` 走 SQLite 官方 backup API，是**真正的原子快照**，且天然包含 WAL 中已提交的内容。对它执行 `.dump`，源库的并发写入再无影响。
- `if` 分支做**能力回退**：`.backup` 需要 SQLite ≥ 3.27（2019-02）。老版本 CLI 会失败，此时退回直接 `.dump` 源库并接受一致性降级（需在日志中明确记录）。
- 用 `sh -c` 而非 `bash -c`：alpine/busybox 环境没有 bash。
- `set -e` 保证任一环节失败即非 0 退出；`if` 条件内的失败不触发 `set -e`，正是回退逻辑需要的语义。
- 结尾的 `printf` 输出 `-- SQLite dump complete`：
  - `--` 是标准 SQL 注释，导入时无副作用；
  - `validate.go` 的 `sql` 分支要求尾部包含 `dump complete`（那是 `pg_dumpall` 的产物），追加这一行即可复用现有校验，**无需修改 `validate.go`**。
- 临时文件路径用 `<slug>`（库名派生）而非 `$$`，保证同一容器内多库不互相覆盖；`rm -f` 前置以满足 `.backup`「目标文件必须不存在」的约束。

### 决策 2：始终按库拆分输出

一个容器常有多个 `.db`。给 `backupProvider` 增加 `forceSingle bool`，`buildPlans` 的条件改为：

```go
if (cfg.singleDBMode || provider.forceSingle) && provider.singleDB != nil {
```

这样无论是否开启 `SINGLE_DB_MODE`，SQLite 都走 `singleDB` 路径，每个库一个文件，复用 `validateDBName`、并发池、重试与清单。

`backupMethod` 仍须实现，用于「枚举失败 → `modeFullFallback`」的兜底：把所有路径 dump 进单一文件。

### 决策 3：默认注入静态 sqlite3，仅注入不可行才失败

「目标容器无 sqlite3」**不再是阻塞项**：备份镜像自带静态 `sqlite3`（§5.6），`sqliteEnsureCLI` 在每次备份前按需 `docker cp` 进目标容器并执行（§4.1）。因此：

- 目标**有** sqlite3 → 直接复用，跳过注入；
- 目标**无** sqlite3 → 注入备份镜像自带的静态二进制，照常产出完整 dump；
- 注入**也失败**（目标无 `/tmp`、根文件系统只读、或 `CopyToContainer` 报错）→ 返回 `permanent` 错误并告警，进入下方显式兜底。

理由：项目在多个环节已确立「宁可失败也不产出可疑备份」的原则（退出码非 0 判失败、0 字节丢弃、校验失败丢弃）。注入方案既满足该原则，又不必在「普遍无 sqlite3」的真实场景放弃备份。

需要最后兜底的用户通过标签**显式开启原始拷贝**（`fallback=copy`）：

```
backup.sqlite.fallback=copy
```

开启后执行 `cat '<db>' '<db>-wal' '<db>-shm'` 合并流式输出（某文件不存在则跳过），同时：

1. 日志 warn 明确提示「原始拷贝无法保证一致性，WAL 内容可能不完整」；
2. `fileExt` 切换为 `db`，并在 `checkBackupStructure` 新增 SQLite 魔数校验。

注意：原始拷贝仍尽量带上 `-wal`/`-shm`，是为了在 WAL 模式下尽可能多保留已提交内容；但它**不提供 `.backup` 的原子快照语义**，并发写入瞬间仍可能撕裂，故仅作显式兜底、默认关闭。

### 决策 4：库路径由标签声明，支持多路径与 glob

| 标签 | 说明 |
|------|------|
| `backup.provider=sqlite` | 必需，声明 provider |
| `backup.sqlite.path` | 必需，逗号分隔；每项可为精确路径或 glob |
| `backup.sqlite.fallback=copy` | 可选，显式开启无 CLI 时的原始拷贝降级 |

展开 glob 在容器内进行：

```sh
for f in <pattern>; do [ -f "$f" ] && printf '%s\n' "$f"; done
```

未匹配时 glob 会退化为字面量，`[ -f ]` 可过滤掉，无需额外判空。

库名取 `basename` 去掉最后一个扩展名（`home-assistant_v2.db` → `home-assistant_v2`），再经 `safeDBName` 校验；含空格等不安全字符的库会被跳过并告警。

---

## 5. 改动清单

### 5.1 `provider.go`

新增：

```go
const labelSQLitePath     = "backup.sqlite.path"
const labelSQLiteFallback = "backup.sqlite.fallback"

// 追加到 backupProvider
forceSingle bool
```

新增函数：

| 函数 | 职责 |
|------|------|
| `sqliteSingleDB` | 读标签 → 展开路径 → 探测 CLI → 为每个库生成 `database` |
| `sqliteBackupCommand` | full 模式回退：所有库 dump 到单一文件 |
| `sqliteDumpScript(src, slug string) string` | 生成 §4.1 的 shell 脚本 |
| `sqliteListScript(patterns []string) string` | 生成 glob 展开脚本 |
| `sqliteDBName(path string) string` | basename 去扩展名 |

注册 provider：

```go
{
	name:         "sqlite",
	patterns:     nil,          // 只能靠标签识别
	fileExt:      "sql",
	forceSingle:  true,
	backupMethod: sqliteBackupCommand,
	singleDB:     sqliteSingleDB,
}
```

### 5.2 `backupProvider` / `database` / `containerPlan` 的 fileExt 覆盖

`fileExt` 目前是 provider 级字段，而降级模式需要 per-database 切换为 `db`。最小侵入做法：

- `database` 增加 `fileExt string`（空则用 provider 默认）
- `containerPlan` 增加 `fileExt string`（full 模式回退用）
- `collectTasks` 两处各加一行 `if x != "" { ext = x }`

### 5.3 `docker.go`

- 新增 `labelSQLitePath` / `labelSQLiteFallback` 常量
- 新增 `containerLabels(ctx, containerID) map[string]string`，带 `labelCache`
- 重构 `containerBackupProviderLabel` 复用该缓存（当前每次调用都触发一次 `ContainerInspect`，`buildPlans` 里每个容器会调用两次）

### 5.4 `backup.go`

`buildPlans` 条件改为支持 `forceSingle`（一行）。

另需处理边界：`forceSingle` 下若所有库名均不合法，`dbs` 为空而 `mode == single`，该容器既不计成功也不计失败（静默无产出）。补充：枚举结果为空时记一条 warning 并计入 `unmatched`。

### 5.5 `validate.go`

`checkBackupStructure` 增加：

```go
case "db":
    if !bytes.HasPrefix(head, []byte("SQLite format 3\x00")) {
        return errors.New("SQLite 备份缺少文件头魔数")
    }
```

仅降级模式会走到，属增量分支，不影响现有 `sql` / `rdb`。

### 5.6 `README.md`

- 「支持的数据库」新增 SQLite 条目
- 新增「SQLite 备份说明」章节，与「Redis / Valkey 备份说明」并列，说明标签用法、一致性机制、无 CLI 时的行为
- 环境变量表无需新增（全部走标签）

### 5.8 `docker-compose.yml`

可选：加一个基于 alpine 的 sqlite 示例服务用于本地验证。非必需。

---

## 6. 恢复演练：不接入

`runRestoreDrill` 目前白名单为 postgres / mysql。SQLite 不接入，理由：

1. 演练容器使用**源镜像**（注意：备份镜像本身现已自带静态 `sqlite3`，但演练故意用源镜像以贴近生产），而这些源镜像大多不含 `sqlite3` CLI，无法执行导入；
2. 固定改用带 sqlite 的基础镜像则需要联网安装，且脱离了「用生产镜像验证」的意义；
3. 在备份容器内做解析校验需引入 SQLite 驱动，触发 cgo（见 §1 非目标）。

保持现状：`runRestoreDrill` 会打印「恢复演练暂不支持该类型，已跳过」，行为正确，无需改动。

---

## 7. 测试方案

复用 `docker_mock_test.go` 的 `fakeAPIClient`，通过 `fake.inspect[id].Container.Config.Labels` 注入标签，`fake.execHandler` 按命令内容返回模拟输出。

需在 `dumpHandler` 中补充分支：`which sqlite3` 的成功/失败两态、`sqlite3 .backup`、`sqlite3 .dump`、glob 展开输出。

| 用例 | 断言 |
|------|------|
| `TestSqliteSingleDB` | 多路径展开正确；命令含 `.backup` 与 `.dump`；结尾带 dump complete 标记 |
| `TestSqliteSingleDBNoCLI` | 无 CLI 且未开启 fallback → 返回错误 |
| `TestSqliteSingleDBFallback` | 开启 fallback → 命令为 `cat`；`fileExt == "db"` |
| `TestSqliteSingleDBGlob` | glob 展开脚本过滤掉未匹配的字面量 |
| `TestSqliteSingleDBNoPath` | 缺 `backup.sqlite.path` → 返回错误 |
| `TestSqliteDBName` | 去扩展名、含空格被 `validateDBName` 拒绝 |
| `TestSqliteDumpScript` | 脚本含 `set -e`；`.backup` 失败时有 `.dump` 回退；退出码语义正确 |
| `TestValidateSQLiteFile` | `db` 扩展的魔数校验通过/拒绝 |
| `TestGetBackupProviderByLabelSQLite` | `backup.provider=sqlite` 命中 |

**集成验证**（建议在实现阶段用真实 SQLite 做一次）：起一个临时容器写入数据，在并发写入过程中执行上述脚本，确认 dump 可完整导入且跨表一致——用于印证 §4.1 关于 `.dump` 快照隔离的论断。

---

## 8. 风险与未决项

| 项 | 说明 | 处置 |
|----|------|------|
| `.dump` 快照隔离 | §3.3 指出逐表读取可能不一致，故设计改为先 `.backup` 再 `.dump` | 实现阶段用真实 SQLite 集成验证 |
| 容器内临时空间 | `.backup` 需在容器 `/tmp` 写入一份 db 大小的快照 | 备份前无法预知；失败会由退出码捕获。可考虑后续加磁盘预检 |
| SQLite < 3.27 | `.backup` 不可用 | 自动回退直接 `.dump`，日志 warn |
| 应用持有排他锁 | 极端情况下 `.backup` 可能超时 | 已有 `BACKUP_TIMEOUT_MINUTES` 与重试覆盖 |
| 库名冲突 | 同一容器两个同名 basename（不同目录） | 文件名会冲突，需在后处理去重或加序号——**待定** |

## 9. 工作量估计

新增约 230 行实现（含 `sqliteEnsureCLI` / `copyFileToContainer`）+ 约 170 行测试 + Dockerfile 静态 sqlite3 构建阶段 + 文档，涉及 6 个源文件（`provider.go`、`docker.go`、`backup.go`、`validate.go`、`config.go`、`Dockerfile`），**不改动落盘主链路**（`backup.go` 的 `writeBackup`、压缩、校验、Kopia、通知全部复用）。
