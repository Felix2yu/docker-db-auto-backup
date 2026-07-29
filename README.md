# docker-db-auto-backup

自动备份 Docker 宿主上所有运行中的数据库容器，支持可选的压缩功能。

## 支持的数据库

- MySQL / MariaDB（包括 linuxserver/mariadb）
- PostgreSQL（包括 TimescaleDB、pgvecto.rs、pgvector、Nextcloud AIO、pgautoupgrade、Immich Postgres VectorChord、PostGIS 等）
- Redis

## 安装

容器需要访问 Docker socket。可以挂载 `/var/run/docker.sock`，或通过 `$DOCKER_HOST` 使用 HTTP 代理提供。

将备份目录挂载到 `/var/backups`（或通过 `$BACKUP_DIR` 覆盖）。备份文件按 `{日期}/{容器名}` 组织。

备份默认在每天凌晨运行。修改 `$SCHEDULE` 可自定义 cron 调度表达式，格式参考 [croniter 文档](https://pypi.org/project/croniter/)。

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `BACKUP_DIR` | `/var/backups` | 备份输出目录 |
| `BACKUP_RETENTION_DAYS` | `0` | 备份保留天数，超过该天数的日期目录会被自动删除。`0` 表示永久保留 |
| `SCHEDULE` | `0 0 * * *` | cron 调度表达式（设为空字符串则立即执行一次） |
| `COMPRESSION` | `plain` | 压缩算法：`gzip` / `lzma` / `xz` / `bz2` / `plain` |
| `SINGLE_DB_MODE` | `false` | 设为 `true` 时每个数据库单独备份为一个文件，用户数据与系统库分离 |
| `APPRISE_URLS` | `-` | 逗号分隔的 [Apprise](https://github.com/caronc/apprise) 通知 URL 列表，备份完成后发送通知 |
| `HEALTHCHECKS_ID` | `-` | [Healthchecks.io](https://healthchecks.io/) Ping ID，备份成功后会向 `{HEALTHCHECKS_HOST}/{HEALTHCHECKS_ID}` 发送 GET 请求 |
| `HEALTHCHECKS_HOST` | `https://hc-ping.com` | Healthchecks 服务器地址，自建实例时修改 |
| `UPTIME_KUMA_URL` | `-` | [Uptime Kuma](https://github.com/louislam/uptime-kuma) Push 完整 URL，备份成功后 GET 请求该地址 |

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

### 通知（Apprise）

备份完成后可通过 [Apprise](https://github.com/caronc/apprise) 发送通知到多种渠道，如 Slack、Discord、Telegram、邮件、Pushover 等。

`APPRISE_URLS` 为逗号分隔的 Apprise URL 列表，例如：

```yml
environment:
  - APPRISE_URLS=slack://token-a/token-b/token-c,mailto://user:pass@gmail.com
```

Apprise 支持 100+ 通知渠道，URL 格式见 [Apprise Wiki](https://github.com/caronc/apprise/wiki)。

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
    image: ghcr.io/realorangeone/db-auto-backup:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ./backups:/var/backups
    environment:
      - APPRISE_URLS=slack://token-a/token-b/token-c
      - SINGLE_DB_MODE=true
```

### 一次性运行

将 `$SCHEDULE` 设为空字符串即可立即执行一次备份，不与外部调度器冲突：

```yml
environment:
  - SCHEDULE=
```
