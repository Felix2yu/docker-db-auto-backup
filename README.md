# docker-db-auto-backup

自动备份 Docker 宿主上所有运行中的数据库容器，支持可选的压缩功能。用 Go 编写，单静态二进制。

## 支持的数据库

- MySQL / MariaDB（包括 linuxserver/mariadb）
- PostgreSQL（包括 TimescaleDB、pgvecto.rs、pgvector、Nextcloud AIO、pgautoupgrade、Immich Postgres VectorChord、PostGIS 等）
- Redis / Valkey

## 安装

容器需要访问 Docker socket。可以挂载 `/var/run/docker.sock`，或通过 `$DOCKER_HOST` 使用 HTTP 代理提供。

将备份目录挂载到 `/var/backups`（或通过 `$BACKUP_DIR` 覆盖）。备份文件按 `{日期}/{容器名}` 组织。

备份默认在每天凌晨运行。修改 `$SCHEDULE` 可自定义 cron 调度表达式（标准 5 段 cron，例如 `0 0 * * *`）。

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `BACKUP_DIR` | `/var/backups` | 备份输出目录 |
| `BACKUP_RETENTION_DAYS` | `0` | 备份保留天数，超过该天数的日期目录会被自动删除。`0` 表示永久保留 |
| `SCHEDULE` | `0 0 * * *` | cron 调度表达式（设为空字符串则立即执行一次） |
| `COMPRESSION` | `plain` | 压缩算法：`gzip` / `lzma` / `xz` / `bz2` / `plain` |
| `SINGLE_DB_MODE` | `false` | 设为 `true` 时每个数据库单独备份为一个文件，用户数据与系统库分离 |
| `SHOUTRRR_URLS` | `-` | 逗号分隔的 [Shoutrrr](https://github.com/containrrr/shoutrrr) 通知 URL 列表，备份完成后发送通知 |
| `HEALTHCHECKS_URL` | `-` | [Healthchecks](https://healthchecks.io/) Ping URL（完整地址，如 `https://hc-ping.com/<uuid>` 或自建 `https://hc.example.com/ping/<uuid>`），程序会自动附加 `/start`、`/fail` 等后缀 |
| `SHOW_PROGRESS` | 自动 | 备份时显示进度条（默认在 TTY 中启用） |
| `NTFY_MARKDOWN` | `true` | ntfy 通知启用 Markdown 渲染（自动为 `ntfy://` 地址追加 `markdown=yes`）。无需时设为 `false`，或直接在 URL 写 `?markdown=yes` |
| `BACKUP_VALIDATE` | `true` | 备份完成后校验文件完整：完整解压并检查 dump 头部标识与完成标记，异常时丢弃该备份并报错 |
| `KOPIA_REPOSITORY_TYPE` | `-` | 启用 Kopia 快照以进行异地备份。仓库类型：`filesystem`（本地路径，配合 `--path=`）、`s3`（配合 `--bucket=` 等）；若使用旧名称 `posix` 会被自动映射为 `filesystem` |
| `KOPIA_PASSWORD` | `-` | Kopia 仓库加密密码（必填，用于创建或连接仓库） |
| `KOPIA_REPOSITORY_FLAGS` | `-` | Kopia 仓库连接/创建参数，空格分隔，例如 `--path=/var/backups/kopia-repo` 或 `--bucket=my-bucket --endpoint=https://s3.example.com` |
| `KOPIA_CREATE_REPOSITORY` | `false` | 设为 `true` 时首次备份自动创建仓库；否则尝试连接已有仓库 |
| `KOPIA_POLICY_COMPRESSION` | `-` | Kopia 压缩算法（如 `zstd` / `pgzip` / `s2`），设置后对仓库启用全局压缩策略 |
| `KOPIA_CONFIG_FILE` | `{BACKUP_DIR}/.kopia/repository.config` | Kopia 仓库配置文件路径（本地持久化后可在删除远程仓库时复用） |

> 启用 Kopia 后，备份文件始终保持 `plain`（不压缩）格式，由 Kopia 负责内容去重与加密，避免"先压缩再加密快照"带来的重复空间浪费。

### 单库备份模式（SINGLE_DB_MODE）

默认情况下，PostgreSQL 使用 `pg_dumpall`、MySQL 使用 `mysqldump --all-databases` 将容器内所有数据库导出到一个文件。

设置 `SINGLE_DB_MODE=true` 后，会逐个枚举数据库并单独备份：

- **用户库** → `{BACKUP_DIR}/{日期}/{容器名}/{库名}.sql{压缩后缀}`
- **系统库** → `{BACKUP_DIR}/{日期}/{容器名}/system/{库名}.sql{压缩后缀}`
- **集群全局对象**（PostgreSQL 单库模式） → `{BACKUP_DIR}/{日期}/{容器名}/system/globals.sql{压缩后缀}`

日期格式为 `YYYY-MM-DD`，例如 `2026-07-29`。

系统数据库识别规则：

| 数据库 | 系统库 |
|--------|--------|
| PostgreSQL | `postgres`, `template0`, `template1` |
| MySQL / MariaDB | `information_schema`, `mysql`, `performance_schema`, `sys` |

### 通知（Shoutrrr）

备份完成后可通过 [Shoutrrr](https://github.com/containrrr/shoutrrr) 发送通知到多种渠道，如 Slack、Discord、Telegram、邮件、ntfy 等。

`SHOUTRRR_URLS` 为逗号分隔的 Shoutrrr URL 列表，例如：

```yml
environment:
  - SHOUTRRR_URLS=slack://token-a/token-b/token-c
```

通知正文以 Markdown 格式发送。支持 Markdown 的渠道（如 Slack、Discord、Telegram 等）会自动渲染嵌套列表，清晰展示每个容器下备份的子库明细。

Shoutrrr 支持多种通知渠道，URL 格式见 [Shoutrrr 文档](https://containrrr.dev/shoutrrr/)。

### 压缩

默认不压缩备份文件（假设底层使用 ZFS 等快照或压缩文件系统）。设置 `$COMPRESSION` 可启用压缩：

- `gzip`
- `lzma` / `xz`
- `bz2`
- `plain`（不压缩，默认值）

### 示例 docker-compose.yml

```yml
services:
  backup:
    build: .
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./backups:/var/backups
    environment:
      - SHOUTRRR_URLS=slack://token-a/token-b/token-c
      - SINGLE_DB_MODE=true
```

### 单个 Kopia（异地备份）

Kopia 自动把当天的备份目录做成内容寻址的快照并推送到远程仓库（支持 S3、B2、GCS、WebDAV、SFTP 等）。仅增加极少环境变量：

```yml
environment:
  - KOPIA_REPOSITORY_TYPE=s3
  - KOPIA_PASSWORD=changeme
  - KOPIA_REPOSITORY_FLAGS=--bucket=db-backup --endpoint=https://s3.eu-central-003.backblazeb2.com
```

- 首次备份时仓库可用 `KOPIA_CREATE_REPOSITORY=true` 自动创建；之后设回 `false` 即可连接已有仓库继续增量备份。
- 由于 Kopia 按内容去重，多天备份只会产生增量空间占用。
- 备份文件在启用 Kopia 时保持 `plain` 格式，由 Kopia 统一加密与压缩（如需压缩设置 `KOPIA_POLICY_COMPRESSION`，如 `zstd`）。
- `KOPIA_CONFIG_FILE` 默认放在备份目录内，丢失时以 `repository.config` 与密码即可重新连接。

### TimescaleDB 与 VectorChord 恢复须知

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
- 建议对这两类库设置 `SINGLE_DB_MODE=true`，走逐库 `pg_dump`（跳过 extensi 兼容格式）并单独保留全局对象 `globals.sql`。
- 镜像名已支持 `timescale/timescaledb*`、`tensorchord/vchord-postgres`、`tensorchord/vchord-suite`、`pgvector/pgvector`、`immich-app/postgres` 等。

### 一次性运行

将 `$SCHEDULE` 设为空字符串即可立即执行一次备份，不与外部调度器冲突：

```yml
environment:
  - SCHEDULE=
```

## 开发与测试

```bash
go build -o db-auto-backup .
go test ./...
```

E2E 测试需要通过 Docker 运行（会启动 postgres / mariadb / mysql / redis 四个容器并执行真实备份）：

```bash
./scripts/test.sh
```