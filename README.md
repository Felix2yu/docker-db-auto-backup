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