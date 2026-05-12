#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import json
import os
import shutil
import sqlite3
import sys
import tempfile
from datetime import UTC, datetime
from pathlib import Path, PurePosixPath
from typing import Any

from huggingface_hub import CommitOperationAdd, CommitOperationDelete, HfApi, hf_hub_download
from huggingface_hub.errors import EntryNotFoundError, RepositoryNotFoundError


ROTATION_MANIFEST_TYPE = "daili_usage_keeper_sqlite_rotation_v1"
DEFAULT_ROTATE_INTERVAL_SECONDS = 3600
DEFAULT_ROTATE_KEEP = 48


def env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def require_env(name: str) -> str:
    value = env(name)
    if not value:
        raise SystemExit(f"missing environment variable: {name}")
    return value


def env_int(name: str, default: int) -> int:
    raw = env(name)
    if not raw:
        return default
    try:
        return max(0, int(raw))
    except ValueError:
        return default


def isoformat_utc(value: datetime) -> str:
    return value.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def floor_time(value: datetime, interval_seconds: int) -> datetime:
    if interval_seconds <= 0:
        return value.astimezone(UTC)
    timestamp = int(value.astimezone(UTC).timestamp())
    bucket = timestamp - (timestamp % interval_seconds)
    return datetime.fromtimestamp(bucket, UTC)


def manifest_path_for(path_in_repo: str) -> str:
    path = PurePosixPath(path_in_repo)
    filename = f"{path.stem}.manifest.json"
    if str(path.parent) == ".":
        return filename
    return str(path.parent / filename)


def history_dir_for(path_in_repo: str) -> str:
    path = PurePosixPath(path_in_repo)
    directory = f"{path.stem}.history"
    if str(path.parent) == ".":
        return directory
    return str(path.parent / directory)


def history_path_for(path_in_repo: str, bucket_time: datetime) -> str:
    base = PurePosixPath(path_in_repo)
    suffix = base.suffix or ".db"
    filename = bucket_time.strftime("%Y%m%dT%H%M%SZ") + suffix
    return f"{history_dir_for(path_in_repo)}/{filename}"


def is_rotation_manifest(payload: Any) -> bool:
    return isinstance(payload, dict) and payload.get("type") == ROTATION_MANIFEST_TYPE


def repo_type() -> str:
    return env("KEEPER_HF_REPO_TYPE", "dataset")


def download_repo_file(token: str, repo_id: str, path_in_repo: str) -> Path | None:
    try:
        downloaded = hf_hub_download(
            repo_id=repo_id,
            repo_type=repo_type(),
            filename=path_in_repo,
            token=token,
        )
    except (EntryNotFoundError, RepositoryNotFoundError):
        return None
    return Path(downloaded)


def load_json(path: Path) -> dict[str, Any] | None:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None


def load_manifest(token: str, repo_id: str, manifest_path: str) -> dict[str, Any] | None:
    manifest_file = download_repo_file(token, repo_id, manifest_path)
    if manifest_file is None:
        return None
    payload = load_json(manifest_file)
    if not is_rotation_manifest(payload):
        return None
    return payload


def latest_snapshot_from_manifest(token: str, repo_id: str, path_in_repo: str) -> Path | None:
    manifest = load_manifest(token, repo_id, manifest_path_for(path_in_repo))
    if not manifest:
        return None
    latest_path = manifest.get("latest_path")
    if not isinstance(latest_path, str) or not latest_path:
        return None
    return download_repo_file(token, repo_id, latest_path)


def download_latest(token: str, repo_id: str, path_in_repo: str, local_path: Path) -> int:
    downloaded = download_repo_file(token, repo_id, path_in_repo)
    if downloaded is None:
        downloaded = latest_snapshot_from_manifest(token, repo_id, path_in_repo)
    if downloaded is None:
        return 0
    local_path.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(downloaded, local_path)
    return 0


def snapshot_time_for(path: Path) -> datetime:
    try:
        return datetime.fromtimestamp(path.stat().st_mtime, UTC)
    except OSError:
        return datetime.now(UTC)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def backup_sqlite(src_path: Path) -> Path:
    tmp = tempfile.NamedTemporaryFile(prefix="keeper-upload-", suffix=".db", delete=False)
    tmp_path = Path(tmp.name)
    tmp.close()
    source = sqlite3.connect(f"file:{src_path}?mode=ro", uri=True)
    try:
        destination = sqlite3.connect(tmp_path)
        try:
            source.backup(destination)
        finally:
            destination.close()
    finally:
        source.close()
    return tmp_path


def upload_snapshot(token: str, repo_id: str, path_in_repo: str, local_path: Path) -> int:
    if not local_path.exists() or local_path.stat().st_size == 0:
        return 0

    rotate_interval = env_int("KEEPER_HF_ROTATE_INTERVAL", DEFAULT_ROTATE_INTERVAL_SECONDS)
    rotate_keep = env_int("KEEPER_HF_ROTATE_KEEP", DEFAULT_ROTATE_KEEP)
    api = HfApi(token=token)
    temp_copy = backup_sqlite(local_path)
    try:
        checksum = sha256_file(temp_copy)
        metadata_path = manifest_path_for(path_in_repo) + ".state.json"
        current_state = load_json(download_repo_file(token, repo_id, metadata_path) or Path("/nonexistent"))
        if current_state and current_state.get("sha256") == checksum:
            return 0

        operations = [
            CommitOperationAdd(path_in_repo=path_in_repo, path_or_fileobj=str(temp_copy)),
            CommitOperationAdd(
                path_in_repo=metadata_path,
                path_or_fileobj=json.dumps(
                    {
                        "updated_at": isoformat_utc(datetime.now(UTC)),
                        "sha256": checksum,
                        "size_bytes": temp_copy.stat().st_size,
                    },
                    ensure_ascii=False,
                    indent=2,
                    sort_keys=True,
                ).encode("utf-8"),
            ),
        ]

        if rotate_interval > 0 and rotate_keep > 0:
            bucket_time = floor_time(snapshot_time_for(temp_copy), rotate_interval)
            rotated_path = history_path_for(path_in_repo, bucket_time)
            manifest_path = manifest_path_for(path_in_repo)
            manifest = load_manifest(token, repo_id, manifest_path) or {}
            previous_paths = [
                item
                for item in manifest.get("retained_paths", [])
                if isinstance(item, str) and item
            ]
            retained_paths = [rotated_path, *[item for item in previous_paths if item != rotated_path]][:rotate_keep]
            delete_paths = [item for item in previous_paths if item not in retained_paths]
            if manifest.get("latest_path") != rotated_path:
                operations.append(CommitOperationAdd(path_in_repo=rotated_path, path_or_fileobj=str(temp_copy)))
                operations.append(
                    CommitOperationAdd(
                        path_in_repo=manifest_path,
                        path_or_fileobj=json.dumps(
                            {
                                "type": ROTATION_MANIFEST_TYPE,
                                "version": 1,
                                "latest_path": rotated_path,
                                "updated_at": isoformat_utc(datetime.now(UTC)),
                                "source_path": path_in_repo,
                                "repo_type": repo_type(),
                                "rotation_interval_seconds": rotate_interval,
                                "retention_count": rotate_keep,
                                "retained_paths": retained_paths,
                                "size_bytes": temp_copy.stat().st_size,
                                "sha256": checksum,
                            },
                            ensure_ascii=False,
                            indent=2,
                            sort_keys=True,
                        ).encode("utf-8"),
                    )
                )
                operations.extend(CommitOperationDelete(path_in_repo=item) for item in delete_paths)

        api.create_commit(
            repo_id=repo_id,
            repo_type=repo_type(),
            operations=operations,
            commit_message="Update daili usage keeper sqlite snapshot",
        )
        return 0
    finally:
        try:
            temp_copy.unlink(missing_ok=True)
        except Exception:
            pass


def main() -> int:
    if len(sys.argv) != 3 or sys.argv[1] not in {"download", "upload"}:
        raise SystemExit("usage: hf_state_snapshot.py [download|upload] /path/to/app.db")

    action = sys.argv[1]
    local_path = Path(sys.argv[2])
    token = require_env("KEEPER_HF_TOKEN")
    repo_id = require_env("KEEPER_HF_REPO_ID")
    path_in_repo = require_env("KEEPER_HF_PATH")

    if action == "download":
        return download_latest(token, repo_id, path_in_repo, local_path)
    return upload_snapshot(token, repo_id, path_in_repo, local_path)


if __name__ == "__main__":
    raise SystemExit(main())
