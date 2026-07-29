#!/usr/bin/env python3
import bz2
import fnmatch
import gzip
import lzma
import os
import secrets
import sys
from dataclasses import dataclass
from datetime import datetime
from io import StringIO
from pathlib import Path
from typing import IO, Callable, Dict, Iterable, Optional

import docker
import pycron
from docker.models.containers import Container
from dotenv import dotenv_values
from tqdm.auto import tqdm


@dataclass
class BackupProvider:
    name: str
    patterns: list[str]
    backup_method: Callable[[Container], str]
    file_extension: str
    single_db_method: Optional[Callable[[Container], list[tuple[str, str, bool]]]] = (
        None
    )


def get_container_env(container: Container) -> Dict[str, Optional[str]]:
    """
    Get all environment variables from a container.

    Variables at runtime, rather than those defined in the container.
    """
    _, (env_output, _) = container.exec_run("env", demux=True)
    return dict(dotenv_values(stream=StringIO(env_output.decode())))


def binary_exists_in_container(container: Container, binary_name: str) -> bool:
    """
    Get all environment variables from a container.

    Variables at runtime, rather than those defined in the container.
    """
    exit_code, _ = container.exec_run(["which", binary_name])
    return exit_code == 0


def temp_backup_file_name() -> str:
    """
    Create a temporary file to save backups to,
    then atomically replace backup file
    """
    return ".auto-backup-" + secrets.token_hex(4)


def open_file_compressed(file_path: Path, algorithm: str) -> IO[bytes]:
    file_path.touch(mode=0o600)

    if algorithm == "gzip":
        return gzip.open(file_path, mode="wb")  # type: ignore
    elif algorithm in ["lzma", "xz"]:
        return lzma.open(file_path, mode="wb")
    elif algorithm == "bz2":
        return bz2.open(file_path, mode="wb")
    elif algorithm == "plain":
        return file_path.open(mode="wb")
    raise ValueError(f"Unknown compression method {algorithm}")


def get_compressed_file_extension(algorithm: str) -> str:
    if algorithm == "gzip":
        return ".gz"
    elif algorithm in ["lzma", "xz"]:
        return ".xz"
    elif algorithm == "bz2":
        return ".bz2"
    elif algorithm == "plain":
        return ""
    raise ValueError(f"Unknown compression method {algorithm}")


def backup_psql(container: Container) -> str:
    env = get_container_env(container)
    user = env.get("POSTGRES_USER", "postgres")
    return f"pg_dumpall -U {user}"


def backup_mysql(container: Container) -> str:
    env = get_container_env(container)

    # The mariadb container supports both
    if "MARIADB_ROOT_PASSWORD" in env:
        auth = "-p$MARIADB_ROOT_PASSWORD"
    elif "MYSQL_ROOT_PASSWORD" in env:
        auth = "-p$MYSQL_ROOT_PASSWORD"
    else:
        raise ValueError(f"Unable to find MySQL root password for {container.name}")

    if binary_exists_in_container(container, "mariadb-dump"):
        backup_binary = "mariadb-dump"
    else:
        backup_binary = "mysqldump"

    return f"bash -c '{backup_binary} {auth} --all-databases'"


def backup_redis(container: Container) -> str:
    """
    Note: `SAVE` command locks the database, which isn't ideal.
    Hopefully the commit is fast enough!
    """
    return "sh -c 'redis-cli SAVE > /dev/null && cat /data/dump.rdb'"


SYSTEM_DATABASES_POSTGRES = frozenset({"postgres", "template0", "template1"})
SYSTEM_DATABASES_MYSQL = frozenset(
    {"information_schema", "mysql", "performance_schema", "sys"}
)


def backup_psql_single(container: Container) -> list[tuple[str, str, bool]]:
    env = get_container_env(container)
    user = env.get("POSTGRES_USER", "postgres")

    exit_code, output = container.exec_run(
        [
            "psql",
            "-U",
            user,
            "-t",
            "-A",
            "-c",
            "SELECT datname FROM pg_database ORDER BY datname",
        ],
        demux=True,
    )
    if exit_code != 0:
        raise RuntimeError(f"Failed to list databases for {container.name}: {output}")

    stdout, _ = output
    if stdout is None:
        return []

    databases = []
    for line in stdout.decode().strip().split("\n"):
        line = line.strip()
        if not line:
            continue
        is_system = line in SYSTEM_DATABASES_POSTGRES
        databases.append((line, f"pg_dump -U {user} -d {line}", is_system))

    return databases


def backup_mysql_single(container: Container) -> list[tuple[str, str, bool]]:
    env = get_container_env(container)

    if "MARIADB_ROOT_PASSWORD" in env:
        auth = "-p$MARIADB_ROOT_PASSWORD"
    elif "MYSQL_ROOT_PASSWORD" in env:
        auth = "-p$MYSQL_ROOT_PASSWORD"
    else:
        raise ValueError(f"Unable to find MySQL root password for {container.name}")

    if binary_exists_in_container(container, "mariadb-dump"):
        backup_binary = "mariadb-dump"
        client_binary = "mariadb"
    else:
        backup_binary = "mysqldump"
        client_binary = "mysql"

    exit_code, output = container.exec_run(
        f"bash -c '{client_binary} -u root {auth} -e \"SELECT SCHEMA_NAME FROM INFORMATION_SCHEMA.SCHEMATA ORDER BY SCHEMA_NAME\" -s --skip-column-names'",
        demux=True,
    )
    if exit_code != 0:
        raise RuntimeError(f"Failed to list databases for {container.name}: {output}")

    stdout, _ = output
    if stdout is None:
        return []

    databases = []
    for line in stdout.decode().strip().split("\n"):
        line = line.strip()
        if not line:
            continue
        is_system = line in SYSTEM_DATABASES_MYSQL
        databases.append((line, f"bash -c '{backup_binary} {auth} {line}'", is_system))

    return databases


BACKUP_PROVIDERS: list[BackupProvider] = [
    BackupProvider(
        name="postgres",
        patterns=[
            "postgres",
            "tensorchord/pgvecto-rs",
            "nextcloud/aio-postgresql",
            "timescale/timescaledb*",
            "pgvector/pgvector",
            "pgautoupgrade/pgautoupgrade",
            "immich-app/postgres",
            "postgis/postgis",
            "kartoza/postgis",
        ],
        backup_method=backup_psql,
        file_extension="sql",
        single_db_method=backup_psql_single,
    ),
    BackupProvider(
        name="mysql",
        patterns=["mysql", "mariadb", "linuxserver/mariadb"],
        backup_method=backup_mysql,
        file_extension="sql",
        single_db_method=backup_mysql_single,
    ),
    BackupProvider(
        name="redis",
        patterns=["redis"],
        backup_method=backup_redis,
        file_extension="rdb",
        single_db_method=None,
    ),
]


BACKUP_DIR = Path(os.environ.get("BACKUP_DIR", "/var/backups"))
SCHEDULE = os.environ.get("SCHEDULE", "0 0 * * *")
SHOW_PROGRESS = sys.stdout.isatty()
COMPRESSION = os.environ.get("COMPRESSION", "plain")
SINGLE_DB_MODE = os.environ.get("SINGLE_DB_MODE", "").lower() in ("true", "1", "yes")
APPRISE_URLS = os.environ.get("APPRISE_URLS", "")


def get_backup_provider(container_names: Iterable[str]) -> Optional[BackupProvider]:
    for name in container_names:
        for provider in BACKUP_PROVIDERS:
            if any(fnmatch.fnmatch(name, pattern) for pattern in provider.patterns):
                return provider

    return None


def get_container_names(container: Container) -> Iterable[str]:
    names = set()
    for tag in container.image.tags:
        registry, image = docker.auth.resolve_repository_name(tag)

        # HACK: Strip "library" from official images
        if registry == docker.auth.INDEX_NAME:
            image = image.removeprefix("library/")

        image, tag_name = image.split(":", 1)
        names.add(image)
    return names


@pycron.cron(SCHEDULE)
def backup(now: datetime) -> None:
    print("Starting backup...")

    docker_client = docker.from_env()
    containers = docker_client.containers.list()

    backed_up_containers = []

    print(f"Found {len(containers)} containers.")

    for container in containers:
        container_names = get_container_names(container)
        backup_provider = get_backup_provider(container_names)
        if backup_provider is None:
            continue

        backed_up = False
        if SINGLE_DB_MODE and backup_provider.single_db_method:
            try:
                db_list = backup_provider.single_db_method(container)
            except Exception as e:
                print(
                    f"Warning: single-db mode failed for {container.name}: {e}, "
                    "falling back to default"
                )
            else:
                for db_name, db_command, is_system in db_list:
                    if is_system:
                        db_dir = BACKUP_DIR / container.name / "system"
                    else:
                        db_dir = BACKUP_DIR / container.name
                    db_dir.mkdir(parents=True, exist_ok=True)

                    backup_file = (
                        db_dir
                        / f"{db_name}.{backup_provider.file_extension}{get_compressed_file_extension(COMPRESSION)}"
                    )
                    backup_temp_file_path = db_dir / temp_backup_file_name()

                    _, output = container.exec_run(db_command, stream=True, demux=True)

                    description = f"{container.name}/{db_name} ({backup_provider.name})"

                    with open_file_compressed(
                        backup_temp_file_path, COMPRESSION
                    ) as backup_temp_file:
                        with tqdm.wrapattr(
                            backup_temp_file,
                            method="write",
                            desc=description,
                            disable=not SHOW_PROGRESS,
                        ) as f:
                            for stdout, _ in output:
                                if stdout is None:
                                    continue
                                f.write(stdout)

                    os.replace(backup_temp_file_path, backup_file)

                    if not SHOW_PROGRESS:
                        print(description)

                backed_up = True

        if not backed_up:
            backup_file = (
                BACKUP_DIR
                / f"{container.name}.{backup_provider.file_extension}{get_compressed_file_extension(COMPRESSION)}"
            )
            backup_temp_file_path = BACKUP_DIR / temp_backup_file_name()

            backup_command = backup_provider.backup_method(container)
            _, output = container.exec_run(backup_command, stream=True, demux=True)

            description = f"{container.name} ({backup_provider.name})"

            with open_file_compressed(
                backup_temp_file_path, COMPRESSION
            ) as backup_temp_file:
                with tqdm.wrapattr(
                    backup_temp_file,
                    method="write",
                    desc=description,
                    disable=not SHOW_PROGRESS,
                ) as f:
                    for stdout, _ in output:
                        if stdout is None:
                            continue
                        f.write(stdout)

            os.replace(backup_temp_file_path, backup_file)

            if not SHOW_PROGRESS:
                print(description)

        backed_up_containers.append(container.name)

    duration = (datetime.now() - now).total_seconds()
    print(
        f"Backup of {len(backed_up_containers)} containers complete in {duration:.2f} seconds."
    )

    if APPRISE_URLS:
        import apprise

        apobj = apprise.Apprise()
        for url in APPRISE_URLS.split(","):
            url = url.strip()
            if url:
                apobj.add(url)

        if apobj.urls:
            container_list = "\n".join(f"  - {name}" for name in backed_up_containers)
            apobj.notify(
                title="数据库备份完成",
                body=(
                    f"成功备份 {len(backed_up_containers)} 个容器，"
                    f"耗时 {duration:.2f} 秒。\n\n"
                    f"已备份容器:\n{container_list}"
                ),
            )


if __name__ == "__main__":
    if os.environ.get("SCHEDULE"):
        print(f"Running backup with schedule '{SCHEDULE}'.")
        pycron.start()
    else:
        backup(datetime.now())
