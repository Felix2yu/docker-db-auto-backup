#!/usr/bin/env bash

set -e

echo "> Build and start all containers..."
docker compose build
docker compose up -d

cleanup() {
  docker compose down -t 3
}
trap cleanup EXIT

echo "> Await postgres..."
until docker compose exec -T psql pg_isready -U postgres
do
  sleep 1
done

echo "> Await mysql..."
until docker compose exec -T mysql bash -c 'mysqladmin ping --protocol tcp -p$MYSQL_ROOT_PASSWORD'
do
  sleep 3
done

run_backup() {
  local compression=$1
  docker compose exec -T \
    -e SCHEDULE= \
    -e COMPRESSION="$compression" \
    -e BACKUP_DIR=/var/backups \
    backup /usr/local/bin/db-auto-backup
}

extension_for() {
  case "$1" in
    gzip) echo ".gz" ;;
    xz) echo ".xz" ;;
    bz2) echo ".bz2" ;;
    *) echo "" ;;
  esac
}

stat_mode() {
  if [ "$(uname)" = "Darwin" ]; then stat -f '%Lp' "$1"; else stat -c '%a' "$1"; fi
}

stat_size() {
  if [ "$(uname)" = "Darwin" ]; then stat -f '%z' "$1"; else stat -c '%s' "$1"; fi
}

assert_four_files() {
  local ext=$1
  local dir
  dir=$(ls -d backups/[0-9]*/ | sort | tail -n 1)

  for name in mariadb-1 mysql-1 psql-1; do
    local f="$dir/docker-db-auto-backup-$name.sql$ext"
    [ -f "$f" ] || { echo "missing: $f"; exit 1; }
    [ "$(stat_mode "$f")" = "600" ] || { echo "bad mode: $f"; exit 1; }
    [ "$(stat_size "$f")" -gt 50 ] || { echo "too small: $f"; exit 1; }
  done

  local rdb="$dir/docker-db-auto-backup-redis-1.rdb$ext"
  [ -f "$rdb" ] || { echo "missing: $rdb"; exit 1; }
  [ "$(stat_mode "$rdb")" = "600" ] || { echo "bad mode: $rdb"; exit 1; }
  [ "$(stat_size "$rdb")" -gt 50 ] || { echo "too small: $rdb"; exit 1; }
}

run_backup plain
assert_four_files ""

for algo in gzip xz bz2; do
  run_backup "$algo"
  assert_four_files "$(extension_for "$algo")"
done

echo "> Kopia posix 仓库快照测试..."

docker compose exec -T backup rm -rf /var/backups/kopia-repo /var/backups/.kopia

docker compose exec -T \
  -e SCHEDULE= \
  -e COMPRESSION=gzip \
  -e BACKUP_DIR=/var/backups \
  -e KOPIA_REPOSITORY_TYPE=posix \
  -e KOPIA_PASSWORD=testpass \
  -e 'KOPIA_REPOSITORY_FLAGS=--path=/var/backups/kopia-repo' \
  -e KOPIA_CREATE_REPOSITORY=true \
  backup /usr/local/bin/db-auto-backup

assert_four_files ""

snapshot_out=$(docker compose exec -T \
  -e KOPIA_PASSWORD=testpass \
  -e KOPIA_CONFIG_FILE=/var/backups/.kopia/repository.config \
  backup kopia snapshot list 2>/dev/null || true)
if [ -z "$snapshot_out" ]; then
  echo "kopia snapshot not found"
  exit 1
fi

echo "> E2E test passed"