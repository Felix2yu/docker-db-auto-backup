# docker-db-auto-backup

自动备份 Docker 宿主上所有运行中的数据库容器，支持可选的压缩功能。用 Go 编写，单静态二进制。

备份产物包含一份 `manifest.json` 清单（记录每个文件的体积与 SHA256），并可选推送到 Kopia 异地仓库、可选执行恢复演练，以验证"备份真的能恢复"而不只是"文件格式合法"。

## 支持的数据库

- MySQL / MariaDB（包括 linuxserver/mariadb、bitnami/mysql、bitnami/mariadb）
- PostgreSQL（包括 TimescaleDB、pgvecto.rs、pgvector、Nextcloud AIO、pgautoupgrade、Immich Postgres VectorChord、PostGIS 等）
- Redis / Valkey（包括 bitnami/redis、bitnami/valkey）
- SQLite（**应用自导出**模式：应用提供 `backup` 子命令输出一致快照，见 [SQLite 备份说明](#sqlite-备份说明)）

未被内置规则识别的镜像，可用容器标签 `backup.provider=postgres|mysql|redis|sqlite` 声明，或用 `BACKUP_IMAGE_PATTERNS_FILE` 扩展（见[镜像识别扩展](#镜像识别扩展)）。

## 安装

容器需要访问 Docker socket。可以挂载 `/var/run/docker.sock`，或通过 `$DOCKER_HOST` 使用 HTTP 代理提供。

将备份目录挂载到 `/var/backups`（或通过 `$BACKUP_DIR` 覆盖）。备份文件按 `{日期}/{容器名}` 组织。

> **时区**：日期目录名取自运行时的本地日期。容器未设置 `TZ` 时默认是 UTC，会导致北京时间凌晨的备份落到**前一天**的目录。镜像默认已设 `TZ=Asia/Shanghai`；如自行覆盖 compose，请务必显式设置 `TZ`，或用 `BACKUP_TZ` 单独指定。

## 命令行

不带参数时按 `SCHEDULE` 调度运行（`SCHEDULE` 为空则立即执行一次后退出）。此外提供以下子命令：

```bash
db-auto-backup run                      # 立即执行一次备份，无需改 SCHEDULE
db-auto-backup list                     # 列出所有备份日期及其状态
db-auto-backup list --date 2026-09-21   # 显示该日期的文件明细与校验和
db-auto-backup verify                   # 离线重跑最新一次备份的校验（比对 SHA256）
db-auto-backup verify --date 2026-09-21
db-auto-backup status                   # 最近一次备份的状态、失败明细与下次计划时间
db-auto-backup help
```

`verify` 会重新计算 SHA256 并与清单比对，同时对 dump 内容做结构校验；发现不一致时以非零码退出，可接入定时巡检。

## 备份流程

```
扫描运行中容器 → 按标签/镜像名识别 provider → 并发 dump（含超时与重试）
  → 内容校验 → 原子落盘 → 写入 manifest.json
  → Kopia 异地快照（可选）→ 恢复演练（可选）→ 保留清理 → 通知 / 心跳
```

**失败语义（重要）**：备份结果分三态。

| 状态 | 判定条件 | 行为 |
|------|----------|------|
| `success` | 所有容器均成功 | 正常推送异地、发送通知 |
| `partial` | 部分容器失败，或异地推送失败 | **成功产出照常推送异地、照常通知**，失败明细随通知与心跳上报 |
| `failed` | 没有任何容器产出备份 | 返回非零码，Healthchecks 上报 `/fail` |

这一设计避免了"1 个库失败导致其余 9 个库的备份既不进异地仓库、也不发通知"的静默丢失问题。调度模式下单轮失败**不会**终止进程，而是在下一个周期自动重试。

## 环境变量

### 基础

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `BACKUP_DIR` | `/var/backups` | 备份输出目录 |
| `SCHEDULE` | `0 0 * * *` | cron 调度表达式（设为空字符串则立即执行一次） |
| `BACKUP_TZ` | 系统本地时区 | 用于日期目录与调度计算的时区（如 `Asia/Shanghai`） |
| `COMPRESSION` | `plain` | 压缩算法：`gzip` / `lzma` / `xz` / `bz2` / `plain` |
| `SINGLE_DB_MODE` | `false` | 设为 `true` 时每个数据库单独备份为一个文件 |
| `BACKUP_RETENTION_DAYS` | `0` | 备份保留天数，超过该天数的日期目录会被自动删除。`0` 表示永久保留 |
| `BACKUP_WORKERS` | `min(CPU 核数, 2)` | 并发备份数。瓶颈在数据库容器与磁盘 IO，默认取较小值以避免冲击线上库 |
| `PUID` / `PGID` | `0` | 备份文件属主 |
| `SHOW_PROGRESS` | 自动 | 备份时显示进度条（默认在 TTY 中启用；并发数大于 1 时自动关闭，避免进度条互相覆盖） |
| `BACKUP_VALIDATE` | `true` | 备份完成后校验文件完整性，异常时丢弃该备份并报错 |

### 可靠性

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `BACKUP_TIMEOUT_MINUTES` | `60` | 单个 dump 的超时上限，防止挂起拖垮整轮调度。支持 `30m` / `2h` 写法 |
| `BACKUP_RETRIES` | `2` | 对 exec 创建、连接类等**非确定性**错误重试的次数；校验失败、退出码非 0 等确定性错误不重试 |
| `BACKUP_RETRY_BACKOFF_SECONDS` | `15` | 重试间隔 |
| `BACKUP_MIN_FREE_SPACE` | `1G` | 备份前检查磁盘可用空间，不足则提前失败并告警。支持 `512M` / `2T` / 纯字节数 |
| `BACKUP_RESTORE_DRILL` | `false` | 备份后把 dump 导入临时容器验证可恢复性（见[恢复演练](#恢复演练)） |
| `BACKUP_CHECKSUM` | `true` | 在清单中记录每个备份文件的 SHA256，供 `verify` 事后比对 |
| `BACKUP_SIZE_DRIFT_RATIO` | `0.5` | 与上次备份对比，体积变化超过该比例时发出警告；容器数减少也会告警 |

### 备份范围

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `BACKUP_INCLUDE_CONTAINERS` | - | 容器名白名单（逗号分隔，支持 `*` 通配），设置后只备份这些容器 |
| `BACKUP_EXCLUDE_CONTAINERS` | - | 容器名黑名单（逗号分隔），优先级高于白名单 |
| `BACKUP_INCLUDE_LABELS` | - | 只备份带有指定标签的容器，支持 `key` 与 `key=value` 两种写法 |

### 凭据

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `POSTGRES_BACKUP_USER` | - | 指定 PostgreSQL 备份账号，优先级高于容器内的 `POSTGRES_USER` / `PGUSER` |
| `MYSQL_BACKUP_USER` | - | 指定 MySQL/MariaDB 备份账号（建议使用只读备份专用账号） |
| `MYSQL_BACKUP_PASSWORD` | - | 上述账号的密码 |

未显式指定时，MySQL 会依次尝试容器内的 `MARIADB_ROOT_PASSWORD`、`MYSQL_ROOT_PASSWORD`、`MYSQL_PASSWORD`；**找不到密码时直接报错**，不会退化成交互式等待输入（否则会永久挂起）。密码通过 `MYSQL_PWD` 环境变量注入，不出现在命令行中。

导出命令固定带 `--single-transaction --routines --events`：

- `--single-transaction` 让 InnoDB 拿到跨表一致快照，同时关掉 mysqldump 默认的锁表行为（不加会在备份期间阻塞线上写入）。需要 `RELOAD`（或 `CONNECTION ADMIN`）与 `TRANSACTION` 权限，且只对事务引擎保证一致性。
- `--routines` / `--events` 默认是**关闭**的，不加就会静默漏掉存储过程、函数和定时事件——dump 文件依然合法，校验和恢复演练都发现不了，只有恢复之后才暴露。需要 `SELECT` on `mysql.*`、`SHOW VIEW` 与 `EVENT`。

用只读备份专用账号时，请一并授予上述权限，否则会因权限不足报错。

### 通知与监控

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `NOTIFY_URLS` | - | 逗号分隔的 [apprise-go](https://github.com/unraid/apprise-go) 通知 URL 列表 |
| `HEALTHCHECKS_URL` | - | [Healthchecks](https://healthchecks.io/) Ping URL，程序会自动附加 `/start`、`/fail` 等后缀；部分失败也会上报 `/fail` 并附带失败明细 |
| `NTFY_MARKDOWN` | `true` | ntfy 通知启用 Markdown 渲染（自动为 `ntfy://` 地址追加 `?format=markdown`） |
| `LOG_FORMAT` | `text` | 日志格式：`text` 或 `json`（结构化日志，便于采集） |
| `LOG_LEVEL` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `METRICS_ADDR` | - | 启用 Prometheus 指标端点，如 `:9090`，暴露 `/metrics` 与 `/healthz` |

指标包括：备份次数、失败次数、上次成功时间戳、耗时、容器数、文件数、总字节数与上次状态（`1` 成功 / `0.5` 部分失败 / `0` 失败）。

### Kopia（异地备份）

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `KOPIA_REPOSITORY_TYPE` | - | 仓库类型：`filesystem`（配合 `--path=`）、`s3`（配合 `--bucket=` 等）；旧名称 `posix` 会自动映射为 `filesystem` |
| `KOPIA_PASSWORD` | - | 仓库加密密码（必填） |
| `KOPIA_REPOSITORY_FLAGS` | - | 仓库连接/创建参数，空格分隔 |
| `KOPIA_CREATE_REPOSITORY` | `false` | 首次备份自动创建仓库 |
| `KOPIA_POLICY_COMPRESSION` | - | 仓库压缩算法（如 `zstd` / `pgzip` / `s2`） |
| `KOPIA_RETENTION_FLAGS` | - | **保留策略**，空格分隔，如 `--keep-daily=7 --keep-weekly=4 --keep-monthly=6` |
| `KOPIA_MAINTENANCE_INTERVAL_HOURS` | `24` | 仓库维护间隔（设为 `0` 关闭）。定期维护可清理过期快照并维持仓库性能 |
| `KOPIA_CONFIG_FILE` | `{BACKUP_DIR}/.kopia/repository.config` | 仓库配置文件路径 |

> 启用 Kopia 后，备份文件始终保持 `plain`（不压缩）格式，由 Kopia 负责内容去重与加密。
>
> 快照会带上 `--description auto-backup {日期}`，快照 ID 记录在当天的 `manifest.json` 中，便于在远端定位某次备份。

### 镜像识别扩展

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `BACKUP_IMAGE_PATTERNS_FILE` | - | JSON 文件，为内置 provider 追加识别规则或注册自定义 dump 命令 |

```json
{
  "patterns": [
    { "pattern": "myorg/postgres-fork", "provider": "postgres" },
    { "pattern": "myorg/mydb", "provider": "mysql", "fileExt": "sql",
      "command": ["mysqldump", "--all-databases"] }
  ]
}
```

只写 `pattern` + `provider` 时，等于给内置 provider 追加一条规则；同时给出 `command` 时注册一个使用固定命令的自定义 provider。

## 单库备份模式（SINGLE_DB_MODE）

默认情况下，PostgreSQL 使用 `pg_dumpall`、MySQL 使用 `mysqldump --all-databases` 将容器内所有数据库导出到一个文件。

设置 `SINGLE_DB_MODE=true` 后，会逐个枚举数据库并单独备份：

- **用户库** → `{BACKUP_DIR}/{日期}/{容器名}/{库名}.sql{压缩后缀}`
- **系统库** → `{BACKUP_DIR}/{日期}/{容器名}/system/{库名}.sql{压缩后缀}`
- **集群全局对象**（PostgreSQL 单库模式） → `{BACKUP_DIR}/{日期}/{容器名}/system/globals.sql{压缩后缀}`

日期格式为 `YYYY-MM-DD`，例如 `2026-07-29`。

系统数据库识别规则（两类数据库都会备份系统库，只是落到 `system/` 目录）：

| 数据库 | 系统库 |
|--------|--------|
| PostgreSQL | `postgres`, `template0`, `template1` |
| MySQL / MariaDB | `information_schema`, `mysql`, `performance_schema`, `sys` |

单库模式下各库也会进入并发队列，不会串行等待。枚举失败时自动回退为全库备份，并在通知中标注"（单库模式回退为全库备份）"。

数据库名只允许 `字母/数字/_/-/.`，含路径分隔符的名称会被跳过，避免路径穿越。

MySQL 单库导出使用 `mysqldump --databases {库名}`，产物自带 `CREATE DATABASE` 与 `USE`，可以直接 `mysql < dump.sql` 导入，无需事先指定目标库（PostgreSQL 的 `pg_dump` 输出本身带 `\connect`）。

## 备份清单（manifest.json）

每次备份都会在日期目录写入 `manifest.json`：

```json
{
  "version": 1,
  "date": "2026-09-21",
  "run_at": "2026-09-21T00:00:03+08:00",
  "status": "success",
  "duration_seconds": 12.4,
  "timezone": "Local",
  "kopia": { "pushed": true, "snapshot_id": "k1a2b3c4" },
  "containers": [
    {
      "name": "pg",
      "provider": "postgres",
      "mode": "single",
      "duration_seconds": 8.1,
      "files": [
        { "path": "2026-09-21/pg/appdb.sql", "database": "appdb", "size": 1048576, "sha256": "..." }
      ]
    }
  ],
  "failures": [],
  "warnings": []
}
```

它同时服务于：`list` / `verify` / `status` 子命令、磁盘容量预估、容器缺失与体积骤变检测。清单位于日期目录内，因此会被 Kopia 一并快照。

## 恢复演练

`BACKUP_RESTORE_DRILL=true` 时，备份完成后会用**源容器正在运行的那个镜像**（按镜像 ID 引用，不是同名仓库的 `:latest`）启动一个临时容器，把 dump 导入并执行一次查询校验，最后连同它的数据卷一起销毁。

这能发现"文件格式合法但无法导入"的问题（扩展版本不匹配、`search_path` 问题等）。锁定镜像 ID 是为了让演练面对的版本与源库一致——否则导入的是"新版本的临时实例"，通过与否都说明不了真实恢复会发生什么。演练失败只作为警告上报，不会让已成功的备份作废。

> SQLite 容器暂不参与恢复演练（演练白名单为 postgres / mysql），其一致性由应用自导出快照保证；如需验证，可直接用 `sqlite3 <备份文件> "PRAGMA integrity_check"` 检查。

注意：演练会临时占用 CPU、内存与磁盘，且向量索引重建可能耗时较长，建议先在非高峰时段试用。

## 通知（Apprise）

备份完成后可通过 [apprise-go](https://github.com/unraid/apprise-go) 发送通知到多种渠道。通知正文为 Markdown，包含状态、耗时、容器数、文件数、总体积、异地快照结果、各容器与库的体积明细，以及失败与警告明细。

ntfy 的 Markdown 渲染由 Apprise 的 `?format=markdown` 控制（本项目在 `NTFY_MARKDOWN=true` 时自动为 `ntfy://` 地址追加该参数）。请勿在 URL 中使用 `?markdown=yes` —— apprise-go 会忽略它，导致消息以纯文本发送、且 Markdown 在转换过程中被损坏。

## 压缩

默认不压缩备份文件（假设底层使用 ZFS 等快照或压缩文件系统）。设置 `$COMPRESSION` 可启用压缩：`gzip` / `lzma` / `xz` / `bz2` / `plain`。

未压缩（`plain`）的备份在校验时只读取文件首尾，不会全量重读，避免大库场景下 IO 翻倍。

## SQLite 备份说明

SQLite 是应用内嵌的文件型数据库，没有服务进程可连、容器里通常也没有 `sqlite3` CLI，因此本项目采用**应用自导出**模式：由应用自己在进程内生成一致快照，写到 stdout，再走与 postgres/mysql 完全相同的落盘、校验、清单、Kopia 与通知管道。

### 为什么不让备份容器直接拷文件

直接复制正在被写入的 `.db`/`-wal`/`-shm`（无论是从宿主机挂载点还是 Docker API）不是原子操作，实测在活跃写入下会周期性产出损坏副本；即使侥幸打开成功，WAL 中已提交的数据也可能丢失。应用进程内用 SQLite 自身的快照能力（`VACUUM INTO` / backup API）导出，是 SQLite 保证的事务级一致视图，WAL 模式下不阻塞写入，也不需要容器内有任何额外二进制或临时空间。

### 识别方式（二选一）

| 方式 | 说明 |
|------|------|
| 容器标签 | `backup.provider: sqlite` |
| 环境变量 | 在应用容器内设置 `BACKUP_SQLITE_CMD`（如 `shenshi backup`），无需标签 |

导出命令：`BACKUP_SQLITE_CMD` 原样生效；未设置时按镜像名推导同名二进制 + `backup`（约定：`ghcr.io/felix2yu/shenshi` → `shenshi backup`）。命令统一经容器内 `sh -c` 执行，因此**推导出的名字必须能被容器内 shell 解析**。本舰队的镜像把二进制装在 `/app`（不在 `PATH` 上），所以镜像名推导在这族镜像上不成立，必须显式声明绝对路径：`BACKUP_SQLITE_CMD=/app/<app> backup`。

两条识别路径能做到的事不同，据实测：

- 打了 `backup.provider: sqlite` 标签：镜像名推导可达，只配标签即可备份（前提是名字能被解析）。
- 不打标签、只靠环境变量：`BACKUP_SQLITE_CMD` 是唯一入口。没设它的容器会被归入下面的"疑似"清单并跳过，**不会**再用镜像名推导去试——宁可告警也不对陌生镜像执行猜出来的命令。

未声明导出命令、但环境变量里出现 `.db`/`.sqlite` 路径或 `sqlite://` DSN 的容器，会被计入"疑似数据库但未能识别"告警，避免静默漏备份。

### 可选环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `BACKUP_SQLITE_CMD` | 镜像名推导 | 自导出命令（空格分隔），设置即视为声明 sqlite 容器 |
| `BACKUP_SQLITE_EXT` | `db` | 产物扩展名。应用把数据与附件一起打成 zip 时设为 `zip`，工具会按 zip 魔数校验并按 `.zip` 命名落盘 |
| `BACKUP_SQLITE_USER` | 由 `PUID`/`PGID` 推导 | exec 导出命令的用户身份（如 `1000:1000`）。容器未设置 `PUID`/`PGID` 时留空，沿用容器默认用户 |

`BACKUP_SQLITE_USER` 的意义：pg/mysql 的 dump 只读网络，用容器默认用户即可；SQLite 的导出进程会碰数据目录——若应用停止时以 root 身份建立只读连接，会创建属主为 root 的 `-shm`/`-wal`，之后降权运行的应用反而打不开自己的库。因此导出进程应尽量与数据属主同身份（`PUID`/`PGID` 是这类镜像的通用约定）。

### 应用侧约定

> 完整契约（`backup` 子命令五条要求、变量语义、exec 身份、恢复语义）已上收
> [`Felix2yu/.github/docs/backup.md`](https://github.com/Felix2yu/.github/blob/main/docs/backup.md)
> 统一维护；本节只留最简实现示例。`scripts/check-docker.py` 按该契约检查各应用仓的 compose 声明。

应用需实现 `backup` 子命令：在进程内把数据库导出为一致快照并写到 stdout。`VACUUM INTO` 是最简实现（Go 示例）：

```go
if len(os.Args) > 1 && os.Args[1] == "backup" {
    dir, _ := os.MkdirTemp("", "app-backup-")
    defer os.RemoveAll(dir)
    target := filepath.Join(dir, "snapshot.db")

    db, err := sql.Open("sqlite", dbPath) // 只读打开即可
    if err != nil { log.Fatal(err) }
    defer db.Close()
    if _, err := db.Exec("VACUUM INTO ?", target); err != nil { log.Fatal(err) }

    f, err := os.Open(target)
    if err != nil { log.Fatal(err) }
    defer f.Close()
    if _, err := io.Copy(os.Stdout, f); err != nil { log.Fatal(err) }
    return
}
```

.NET / Python / Node 等运行时的 SQLite 绑定同样提供在线备份 API（如 Python 的 `sqlite3.Connection.backup()`）。

### 限制与注意

- **一个容器一个文件**：产物为 `{BACKUP_DIR}/{日期}/{容器名}.{ext}`，与 `SINGLE_DB_MODE` 无关。默认 `ext=db`（SQLite 快照，按 `SQLite format 3\0` 魔数校验）；输出 zip（数据 + 附件一起打包）时用 `BACKUP_SQLITE_EXT=zip`（按 `PK\x03\x04` 魔数校验）。**扩展名必须与产物一致**：应用输出 zip 却按默认 `db` 校验会失败并丢弃产物（这是有意为之，不允许把 zip 当 SQLite 存下来）。
- **`backup` 子命令应在应用 `main()` 最前面拦截**，早于任何存储初始化/迁移，避免 exec 进程重复初始化或以错误身份写数据目录。
- **exec 身份**：`docker exec` 不经过 ENTRYPOINT，默认以容器配置的用户运行（通常是 root）。建议按上文设置 `BACKUP_SQLITE_USER` 或让 compose 提供 `PUID`/`PGID`，避免 exec 进程在数据目录里创建属主错误的文件。
- **恢复**：`db` 产物放回数据目录并重启应用即可（`VACUUM INTO` 产物的 `journal_mode` 为默认 delete 模式，应用启动时会按自身逻辑重新切换 WAL）；`zip` 产物用应用自己的导入功能恢复（如 Web UI 上传）。

## Redis / Valkey 备份说明

Redis 使用 `BGSAVE`（后台异步落盘）而非同步 `SAVE`，不会阻塞 Redis 主线程；随后轮询 `INFO persistence` 等待落盘完成，超时（120 秒）则判定失败。

等待结束并不等于拿到了新快照：BGSAVE 因磁盘满或 fork 失败而没真正落盘时，`rdb_bgsave_in_progress` 同样会立刻归零，而 Redis 只在成功时才把临时文件改名成 `dbfilename`，盘上留的是**上一次成功保存的旧 RDB**——魔数与结束标记都合法，光看文件内容分辨不出来。因此脚本还会核对 `rdb_last_bgsave_status` 不为 `err`、且 `rdb_last_save_time` 相比本次备份开始前确实前进了（除非本来就没有未落盘的改动），否则判定失败并拒绝交付陈旧文件。RDB 路径通过 `CONFIG GET dir` / `CONFIG GET dbfilename` 动态获取，不再硬编码 `/data/dump.rdb`。

## 示例 docker-compose.yml

```yml
services:
  backup:
    build: .
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./backups:/var/backups
    environment:
      - TZ=Asia/Shanghai
      - NOTIFY_URLS=slack://token-a/token-b/token-c
      - SINGLE_DB_MODE=true
      - BACKUP_RETENTION_DAYS=14
```

### 启用 Kopia（异地备份）

```yml
environment:
  - KOPIA_REPOSITORY_TYPE=s3
  - KOPIA_PASSWORD=changeme
  - KOPIA_REPOSITORY_FLAGS=--bucket=db-backup --endpoint=https://s3.eu-central-003.backblazeb2.com
  - KOPIA_RETENTION_FLAGS=--keep-daily=7 --keep-weekly=4 --keep-monthly=6
```

- 首次备份时仓库可用 `KOPIA_CREATE_REPOSITORY=true` 自动创建；之后设回 `false` 即可连接已有仓库继续增量备份。
- 由于 Kopia 按内容去重，多天备份只会产生增量空间占用。
- 设置保留策略后，远端仓库会按策略清理过期快照；未设置时快照只增不减。

### 一次性运行

将 `$SCHEDULE` 设为空字符串即可立即执行一次备份，不与外部调度器冲突：

```yml
environment:
  - SCHEDULE=
```

## TimescaleDB 与 VectorChord 恢复须知

这两个扩展的**逻辑备份（pg_dump/pg_dumpall）可以导出全部数据**，但恢复步骤与普通 PG 不同，请务必按下方核对：

**TimescaleDB（超表、连续聚合、压缩块）**
- 恢复目标库必须 `CREATE EXTENSION timescaledb`，且扩展版本与备份一致（不一致会在 `timescaledb_post_restore()` 时报 `catalog version mismatch`，恢复不可用）。
- 规范顺序：新库 → `CREATE EXTENSION` → `SELECT timescaledb_pre_restore();`（停止后台任务）→ `pg_restore`（**不要加 `-j`**）→ `SELECT timescaledb_post_restore();`（校验目录并重启任务）。
- 直接 `psql < dump` 恢复超表时，后台压缩/保留任务可能在恢复中途运行，导致 catalog 与数据不一致。
- 官方最佳实践是**逐库 pg_dump/restore**，而非 `pg_dumpall`。对 `timescale/timescaledb*` 容器建议开启 `SINGLE_DB_MODE=true`。

**VectorChord / pgvector（vchord、vector 扩展）**
- 数据存储在 PostgreSQL 内，逻辑备份不丢数据；但 **HNSW/IVF 索引不会随备份导出**，恢复时会重新执行 `CREATE INDEX`，向量表越大耗时越长（可能数小时且占用大内存），请提前评估 RTO。
- pg_dump 输出的 `set_config('search_path', '', false)` 会导致恢复时 `type "vector" does not exist`。对这类容器恢复，需要在导入前把该行改写为：
  ```bash
  sed "s/SELECT pg_catalog.set_config('search_path', '', false);/SELECT pg_catalog.set_config('search_path', 'public, pg_catalog', true);/g"
  ```
- 依赖向量检索的应用（如 Immich）恢复时不要重启应用与导入并行，避免应用自己的迁移冲突。
- 若看重恢复速度，对这类库使用数据目录物理备份（`pg_basebackup` / 文件快照）可免去索引重建。

**通用**
- 建议对这两类库设置 `SINGLE_DB_MODE=true`，走逐库 `pg_dump` 并单独保留全局对象 `globals.sql`。
- 镜像名已支持 `timescale/timescaledb*`、`tensorchord/vchord-postgres`、`tensorchord/vchord-suite`、`pgvector/pgvector`、`immich-app/postgres` 等。

## 开发与测试

```bash
go build -o db-auto-backup .
go test ./...
./scripts/lint.sh
```

E2E 测试需要通过 Docker 运行（会启动 postgres / mariadb / mysql / redis 四个容器并执行真实备份）：

```bash
./scripts/test.sh
```

## 许可证

本项目以 [BSD 3-Clause](LICENSE) 发布，© 2021 Jake Howard。
