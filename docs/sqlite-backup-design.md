# SQLite 备份支持 —— 详细设计

## 1. 目标与范围

为 `docker-db-auto-backup` 增加 SQLite 备份能力，覆盖「应用容器内嵌 SQLite 文件」的场景（自建服务、Vaultwarden、Home Assistant 这类应用）。

**导出方式：应用自导出。** 应用提供 `backup` 子命令，在进程内用 SQLite 自己的快照能力（`VACUUM INTO` / backup API）生成一致副本并写到 stdout，工具侧完全复用既有管道：

```
buildPlans → provider.prepare / provider.backupMethod
  → collectTasks（展开为最小调度单元）
  → runTaskWithRetry（并发 + 超时 + 重试）
  → writeBackup（stdout 流式 → 临时文件 → 压缩 → 校验 → 原子 rename）
  → manifest / 异常检测 / Kopia / 恢复演练 / 保留清理 / 通知 / 心跳
```

**非目标**：

- 不做宿主机上的 SQLite 备份（本项目只处理容器）。
- 不引入 Go 的 SQLite 驱动（不校验库内容、不做恢复演练，理由见 §6）。
- 不强求容器内有 `sqlite3` CLI（绝大多数应用镜像没有）。

---

## 2. 为什么是「应用自导出」

### 2.1 排除：容器内 CLI（`sqlite3 .backup` / `.dump`）

- 应用镜像普遍不带 `sqlite3`；注入静态二进制需要 `CopyToContainer`，并在目标容器无 `/tmp`、根文件系统只读时失败——依赖链最长、失败面最大。
- `.backup /dev/stdout` 实测不可用（`Error: cannot open`），必须先在容器内落临时文件再 `cat`，多一份容器内空间占用与清理责任。
- `.dump` 是逐表顺序读取，autocommit 下没有单一快照，跨表可能自相矛盾。

### 2.2 排除：从容器外热拷贝 `db`/`-wal`/`-shm`

实测（20ms 写一次、压测 `integrity_check`）：

| 拷贝内容 | 通过率 |
|----------|--------|
| db + wal + shm | 14/15 |
| db + wal | 12/15 |
| 仅 db | 11/15 |

低频写入（200ms 一次）30/30 通过——说明它在「安静」时好用，在活跃写入时**周期性产出损坏副本**。作为统一备份体系的基础不能接受，也不值得用「校验 + 重试 + 失败报错」把它兜成可用。

### 2.3 采用：应用自导出

| 维度 | 应用自导出 |
|------|------------|
| 一致性 | SQLite 自身的事务级快照，WAL 模式下也不阻塞写入 |
| 依赖 | 无：镜像里没有任何额外二进制，也不需要容器内临时空间（应用侧自管） |
| 路径解析 | 应用最清楚自己的库在哪（`DB_PATH`/`DATA_DIR` 等），不靠 glob 启发 |
| 产物语义 | 应用可自行决定「带附件的完整备份」（如 shenshi 的 zip）还是裸库快照 |
| 契约 | 显式、可单测、失败模式最少 |

代价是每个应用要加约 30 行代码（见 §7.2），换来的是把一致性责任交给唯一有资格承担的一方。

---

## 3. 识别方式

`sqlite` provider 的 `patterns` 为空：SQLite 没有官方镜像，镜像名匹配没有可靠依据。识别只有两条路：

| 方式 | 说明 |
|------|------|
| 容器标签 | `backup.provider=sqlite`（既有机制，注册 provider 后自动生效） |
| 环境变量 | 容器设置 `BACKUP_SQLITE_CMD`（如 `shenshi backup`）即视为声明，`probeSQLiteContainer` 在计划阶段探测 |

命令解析优先级：`BACKUP_SQLITE_CMD` > 镜像名同名二进制 + `backup`（`ghcr.io/owner/app:tag` → `app backup`，取 basename 并用 `sqliteBinaryNamePattern` 约束字符集，防 shell 注入）。命令统一经容器内 `sh -c` 执行——`docker exec` 的任务命令不保证做 PATH 查找，因此推导出的名字必须在容器 `PATH` 上；基镜契约本就要求 `COPY bin/app /usr/local/bin/app`，守约的镜像无需任何额外配置。

**两条识别路径能力不对称**（实测，非设计意图）：镜像名推导只在打了 `backup.provider=sqlite` 标签时可达。不打标签时，识别完全依赖 `BACKUP_SQLITE_CMD`——没设它的容器会先被 `probeSQLiteContainer` 判为"疑似"并 `continue`，永远走不到推导分支（`buildPlans`）。这是有意的保守取舍：不对陌生镜像执行猜出来的命令，代价是应用实现了 `backup` 子命令却漏配变量时只会告警、不会备份。

**防漏备份**：未声明导出命令、但环境变量值里出现 `.db`/`.sqlite`/`.sqlite3` 后缀或 `sqlite://` DSN 的容器，会被计入 `unmatched`，在日志与通知里提示「疑似 SQLite 但未能识别」，而不是静默跳过。探测走 `containerEnv`（带缓存、10s 超时、失败静默），不改变其它容器的行为。

---

## 4. 产物与校验

- 默认产物 `{BACKUP_DIR}/{日期}/{容器名}.db`（full 模式，一个容器一个文件）。
- `BACKUP_SQLITE_EXT` 覆盖扩展名：应用导出「数据 + 附件」的 zip 时设为 `zip`，工具按 `PK\x03\x04` 校验、按 `.zip` 命名。扩展名经 `sqliteFileExtPattern`（字母数字、≤16 位）约束。
- `validate.go` 的 `checkBackupStructure` 增加两个分支：`db` 校验 `SQLite format 3\0` 魔数；`zip` 校验本地文件头魔数。
- 扩展名与产物不一致时（如忘了 `BACKUP_SQLITE_EXT=zip`），校验失败、产物丢弃、备份标记失败——不允许把 zip 当 SQLite 存下来。

---

## 5. exec 身份

pg/mysql 的 dump 进程只读网络，用容器默认用户即可；SQLite 的导出进程会碰数据目录：**应用停止时一个 root 身份的连接会创建属主为 root 的 `-shm`/`-wal`，之后降权运行的应用反而打不开自己的库。**

因此新增 `sqliteExecUser`，按优先级决定 `ExecCreateOptions.User`：

1. `BACKUP_SQLITE_USER`（显式，形如 `1000:1000` 或用户名）
2. `PUID`/`PGID`（同时存在且为纯数字时拼成 `uid:gid`）
3. 留空：沿用容器默认用户（与其它 provider 一致）

该逻辑仅挂在 sqlite provider 的 `prepare` 钩子上，不改动既有 provider 的执行行为。

---

## 6. 恢复与恢复演练

- `db` 产物：放回数据目录、修属主、重启应用（`VACUUM INTO` 产物为 delete 日志模式，应用启动会按自身逻辑切回 WAL）。
- `zip` 产物：用应用自己的导入功能恢复（如 shenshi 的上传导入）。
- **不接入恢复演练**：演练容器使用源镜像，而源镜像没有 sqlite3 CLI；在备份容器内解析需要引入 SQLite 驱动。`runRestoreDrill` 会打印「恢复演练暂不支持该类型，已跳过」，行为正确。

---

## 7. 改动清单（已落地）

### 7.1 工具侧

| 文件 | 内容 |
|------|------|
| `provider.go` | sqlite provider 注册；`sqliteBackupCommand` / `sqliteBinaryName` / `probeSQLiteContainer` / `sqlitePathLikeValue` / `prepareSQLitePlan` / `sqliteFileExt` / `sqliteExecUser` |
| `backup.go` | `containerPlan` 增加 `fileExt` / `execUser`；`buildPlans` 调用 `provider.prepare` 并接入 env 探测与疑似告警；`collectTasks` 改用计划级扩展名；`writeBackup` 透传 exec 用户 |
| `docker.go` | `startExec` 支持 `User` |
| `validate.go` | `db` / `zip` 魔数校验 |
| `README.md` | 「SQLite 备份说明」章节（识别、可选变量、应用侧约定、限制） |
| 测试 | `sqlite_test.go`：命令推导、env 覆盖、注入防御、探测、标签/ env 识别、疑似告警、db 与 zip 的端到端落盘、两种魔数校验 |

### 7.2 应用侧约定

应用需实现 `backup` 子命令：

- 在 `main()` 最前面拦截，早于存储初始化/迁移（导出进程不该改 schema）；
- 只读/不迁移地打开数据库；数据文件不存在时报错，不建目录、不产空库；
- `VACUUM INTO` 到临时目录，再把文件流式写到 stdout；
- 日志走 stderr，stdout 只承载备份数据；
- 命令必须能被容器内的 `sh -c` 解析：二进制在 `PATH` 上写裸名，否则用绝对路径。

各应用可在此基础上扩展：shenshi 的 `shenshi backup` 默认输出「JSON + 附件」的 zip（可经界面导入还原），`--format db` 输出 SQLite 快照；导出打开方式为 `store.OpenForExport`（跳过迁移与种子写入）。

已接入的应用（验证口径见下）。导出命令一律是 `<二进制名> backup`：

| 应用 | 产物 | 库外文件 | 容器级实测 |
|------|------|----------|------------|
| shenshi | zip（`BACKUP_SQLITE_EXT=zip`） | 附件已含 | 是 |
| feixi / chaxin / guanfu | db | 无库外状态 | 是 |
| liuxia / yuexi | db | 无库外状态 | 是 |
| qiansi | db，zip 可选 | uploads 仅在 zip 形态下覆盖 | 是（db 与 zip 两态） |
| mujian | db，zip 可选 | uploads 仅在 zip 形态下覆盖 | 是（db 与 zip 两态） |

> **命令写法的统一**：liuxia / yuexi / mujian 原先把二进制装在 `/app`（不在 PATH 上），
> 导出命令必须写绝对路径，与其余五个不一致。基镜契约本来就是
> `COPY bin/app /usr/local/bin/app` + `CMD ["app", ...]`，三者已改回契约形态，
> 于是全族命令同为 `<bin> backup`。副作用是镜像名推导从此对每个应用都成立——
> 实测一个只打 `backup.provider=sqlite` 标签、不设任何 `BACKUP_SQLITE_*` 的
> liuxia 容器可正常识别并备份成功。
>
> mujian 的镜像改动需要真实 cgo 二进制才能验证（webp/avif 无法 `CGO_ENABLED=0`
> 交叉编译）：用 `golang:1.27-bookworm` 容器挂宿主 module cache 离线编译出
> linux/arm64 二进制后起容器实测，`/healthz` 正常，`db` 与 `zip` 两态产物均通过
> （zip 内含 `data.json`、`mujian.db` 与 `covers/` 封面文件，字节一致）。

---

## 8. 测试与验证

**单元测试**（`go test ./...`，含新用例）：

| 用例 | 断言 |
|------|------|
| `TestSqliteBackupCommandFromImageName` | 镜像名约定 → `sh -c "app backup"` |
| `TestSqliteBackupCommandEnvOverride` | `BACKUP_SQLITE_CMD` 原样生效 |
| `TestSqliteBackupCommandEnvReadErrorFallsBack` | env 不可读时回退镜像名推导 |
| `TestSqliteBackupCommandNoBinary` | 无法推导时报错并提示声明变量 |
| `TestSqliteBinaryNameRejectsUnsafe` | shell 元字符被拒绝 |
| `TestProbeSQLiteContainer` / `sqlitePathLikeValue` | 声明与疑似两态、DSN 与后缀识别 |
| `TestBuildPlansSQLiteByLabel|ByEnv|LikelyUnmatched` | 标签、env 识别；疑似容器进告警清单 |
| `TestSqliteFileExtOverride` | 扩展名覆写与非法值忽略 |
| `TestSqliteExecUser` | 显式 > PUID/PGID > 默认 |
| `TestBackupContainerSQLiteEndToEnd` / `...ZipEndToEnd` | 完整链路：识别 → exec → 校验 → 落盘 → 清单 |
| `TestValidateBackupSQLiteMagic` / `...ZIP` | 魔数通过与拒绝 |

**真实容器端到端**（本机 Docker，验证记录）：

1. shenshi 容器（用户 1000，WAL 模式）运行中经 `docker exec` 导出：`PRAGMA integrity_check` 通过，刚提交（尚在 `-wal` 中）的任务出现在快照里；
2. 工具以 `BACKUP_SQLITE_CMD` 自动识别并完成备份：zip 与 db 两种产物均落盘、校验通过、清单记录 `provider: sqlite`；
3. exec 身份：用 `BACKUP_SQLITE_CMD="id > /data/identity.txt; shenshi backup --format db"` 让导出进程自己回报身份，三种配置（显式 `BACKUP_SQLITE_USER`、仅 `PUID`/`PGID`、两者都不设）都是 `uid=1000(app)`——公共基镜烘入了 `ENV PUID=1000 PGID=1000`，所以零配置也能与数据属主一致。作为对照，`docker exec <c> id` 不带 `-u` 是 `uid=0(root)`，即镜像默认 exec 身份确实是 root，派生逻辑不是可有可无的；
4. 并发写压：导出期间持续写入数百次，快照 `integrity_check` 为 ok，最近一次已提交（仍在 `-wal`）的行被包含，快照行数（413）与实时行数（709）之差即导出瞬间之后新写的数据——中途一致视图，无撕裂；
5. 恢复：备份的 zip 经应用「导入备份」还原（任务与附件都回来）；把 `.db` 快照放回数据目录后应用正常启动并读到全部行，并按自身逻辑切回 WAL；
6. 反例：应用输出 zip 而未声明 `BACKUP_SQLITE_EXT=zip` → 校验失败、产物丢弃、本轮标记 failed（不静默存坏备份）；
7. 压缩叠加：`COMPRESSION=gzip` 下 zip / db 产物均通过——校验先解压再查内层魔数（`validateBackupFile`）；
8. 标签路径：只打 `backup.provider=sqlite`、不给任何 `BACKUP_SQLITE_*` 变量，工具按镜像名推出 `shenshi backup` 并真实执行（产物是 zip，被默认 `.db` 校验拒绝，恰好反证命令确实跑了；补 `BACKUP_SQLITE_EXT=zip` 后成功落盘）；
9. 恢复演练：sqlite 容器跳过，`LOG_LEVEL=debug` 下可见「恢复演练暂不支持该类型，已跳过 provider=sqlite」（info 级不可见）；
10. 多应用同轮：feixi / chaxin / guanfu / liuxia / yuexi / qiansi / qiansi（zip 态）/ shenshi 八个容器在同一轮里 `database=8 skipped=0`，`COMPRESSION=gzip` 下 8/8 成功；`.db` 逐个解开后魔数、`integrity_check`、schema 齐全，`.zip` 解开后确认包内快照与 `uploads/` 文件字节一致，扩展名按各容器声明分别落盘；
11. 压缩叠加第二种算法：`COMPRESSION=xz` 下同样 5/5 通过；
12. 还原确认：把五个应用备份出的 `.db` 分别放进全新数据卷、属主交给 1000、删掉旁文件后启动应用，五个都正常起来并读到自己的数据（sqlite 不参与恢复演练，这一步是手工补的）。

> 修正一条早期记录：原写「`kill -9` 后删除 `-shm`，工具重建的 `-shm` 属主正确」。实测复现不出该场景——导出进程干净退出时会把 `-wal` checkpoint 回主库并删除 `-wal`/`-shm`，因此不会留下任何属主错误的旁文件。身份派生本身仍然有效（见第 3 条），这也是它存在的理由：若导出进程中途被杀，root 身份建的 `-shm` 会挡住降权运行的应用。

---

## 9. 已知边界

| 项 | 说明 |
|----|------|
| 一个容器一个文件 | full 模式语义；多库容器建议应用把多库打包（zip/tar）后输出 |
| 应用必须供命令 | 镜像不提供 `backup` 子命令、也无 `sqlite3` CLI 时无法备份，只能等应用侧支持 |
| `-shm` 属主 | 由 `BACKUP_SQLITE_USER`/`PUID` 保障；两者都缺失时沿用容器默认用户（与既有 provider 一致） |
| 源库只读检查点 | 导出连接可能把 `-wal` 合并回主库（SQLite 正常行为），与属主同身份时无副作用 |
