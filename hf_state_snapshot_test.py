import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import hf_state_snapshot as snapshot


class RotationRetentionTest(unittest.TestCase):
    def test_default_rotation_bucket_matches_upload_interval(self) -> None:
        self.assertEqual(snapshot.DEFAULT_ROTATE_INTERVAL_SECONDS, 60)

    def test_retention_keeps_current_and_newest_existing_paths(self) -> None:
        path = "usage-keeper/app.db"
        retained = snapshot.retained_history_paths(
            path,
            "usage-keeper/app.history/20260522T030000Z.db",
            [
                "usage-keeper/app.history/20260522T020000Z.db",
                "usage-keeper/app.history/20260522T010000Z.db",
            ],
            [
                "usage-keeper/app.history/20260522T020000Z.db",
                "usage-keeper/app.history/20260522T010000Z.db",
                "usage-keeper/app.history/20260522T000000Z.db",
            ],
            3,
        )

        self.assertEqual(
            retained,
            [
                "usage-keeper/app.history/20260522T030000Z.db",
                "usage-keeper/app.history/20260522T020000Z.db",
                "usage-keeper/app.history/20260522T010000Z.db",
            ],
        )

    def test_cleanup_removes_orphaned_history_files_from_repo_listing(self) -> None:
        path = "usage-keeper/app.db"
        delete_paths = snapshot.deleted_history_paths(
            path,
            previous_paths=[
                "usage-keeper/app.history/20260522T020000Z.db",
                "usage-keeper/app.history/20260522T010000Z.db",
            ],
            existing_paths=[
                "usage-keeper/app.history/20260522T020000Z.db",
                "usage-keeper/app.history/20260522T010000Z.db",
                "usage-keeper/app.history/20260522T000000Z.db",
                "usage-keeper/other.history/20260522T000000Z.db",
            ],
            retained_paths=[
                "usage-keeper/app.history/20260522T020000Z.db",
                "usage-keeper/app.history/20260522T010000Z.db",
            ],
            repo_listing_available=True,
        )

        self.assertEqual(delete_paths, ["usage-keeper/app.history/20260522T000000Z.db"])

    def test_upload_cleanup_does_not_overwrite_latest_path_when_rotating(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            temp_copy = Path(tmpdir) / "app.db"
            temp_copy.write_bytes(b"sqlite snapshot")
            checksum = hashlib.sha256(temp_copy.read_bytes()).hexdigest()
            state_file = Path(tmpdir) / "state.json"
            state_file.write_text(json.dumps({"sha256": checksum}), encoding="utf-8")
            local_db = Path(tmpdir) / "local.db"
            local_db.write_bytes(b"source")

            class FakeApi:
                def __init__(self) -> None:
                    self.operations = []

                def create_commit(self, **kwargs) -> None:
                    self.operations = kwargs["operations"]

            fake_api = FakeApi()

            def fake_download(_token: str, _repo_id: str, path_in_repo: str):
                if path_in_repo.endswith(".state.json"):
                    return state_file
                return None

            with (
                patch.object(snapshot, "HfApi", return_value=fake_api),
                patch.object(snapshot, "backup_sqlite", return_value=temp_copy),
                patch.object(snapshot, "download_repo_file", side_effect=fake_download),
                patch.object(
                    snapshot,
                    "list_repo_files_safe",
                    return_value={
                        "usage-keeper/app.db",
                        "usage-keeper/app.history/20260522T030000Z.db",
                        "usage-keeper/app.history/20260522T020000Z.db",
                    },
                ),
                patch.object(
                    snapshot,
                    "load_manifest",
                    return_value={
                        "type": snapshot.ROTATION_MANIFEST_TYPE,
                        "latest_path": "usage-keeper/app.history/20260522T030000Z.db",
                        "rotation_interval_seconds": 3600,
                        "retention_count": 48,
                        "retained_paths": [
                            "usage-keeper/app.history/20260522T030000Z.db",
                            "usage-keeper/app.history/20260522T020000Z.db",
                        ],
                    },
                ),
                patch.object(snapshot, "prune_lfs_storage_if_due"),
            ):
                self.assertEqual(snapshot.upload_snapshot("token", "repo", "usage-keeper/app.db", local_db), 0)

            added_paths = {
                operation.path_in_repo
                for operation in fake_api.operations
                if isinstance(operation, snapshot.CommitOperationAdd)
            }
            deleted_paths = {
                operation.path_in_repo
                for operation in fake_api.operations
                if isinstance(operation, snapshot.CommitOperationDelete)
            }

            self.assertNotIn("usage-keeper/app.db", added_paths)
            self.assertIn("usage-keeper/app.db", deleted_paths)
            self.assertIn("usage-keeper/app.manifest.json", added_paths)

    def test_stale_lfs_files_excludes_current_tree_oids(self) -> None:
        class FakeApi:
            def list_repo_tree(self, **_kwargs):
                return [
                    SimpleNamespace(lfs=SimpleNamespace(sha256="keep-1")),
                    SimpleNamespace(lfs=SimpleNamespace(sha256="keep-2")),
                    SimpleNamespace(lfs=None),
                ]

            def list_lfs_files(self, *_args, **_kwargs):
                return [
                    SimpleNamespace(file_oid="keep-1", size=10),
                    SimpleNamespace(file_oid="stale-1", size=20),
                    SimpleNamespace(file_oid="keep-2", size=30),
                ]

        stale, stale_bytes, total_count, current_count = snapshot.stale_lfs_files(FakeApi(), "token", "repo")

        self.assertEqual([item.file_oid for item in stale], ["stale-1"])
        self.assertEqual(stale_bytes, 20)
        self.assertEqual(total_count, 3)
        self.assertEqual(current_count, 2)

    def test_prune_lfs_storage_deletes_only_stale_lfs_and_records_marker(self) -> None:
        class FakeApi:
            def __init__(self) -> None:
                self.deleted = []
                self.marker_operations = []

            def list_repo_tree(self, **_kwargs):
                return [SimpleNamespace(lfs=SimpleNamespace(sha256="current"))]

            def list_lfs_files(self, *_args, **_kwargs):
                return [
                    SimpleNamespace(file_oid="current", size=10),
                    SimpleNamespace(file_oid="old", size=20),
                ]

            def permanently_delete_lfs_files(self, _repo_id, lfs_files, **_kwargs) -> None:
                self.deleted = list(lfs_files)

            def create_commit(self, **kwargs) -> None:
                self.marker_operations = kwargs["operations"]

        fake_api = FakeApi()
        with patch.object(snapshot, "lfs_prune_due", return_value=True):
            snapshot.prune_lfs_storage_if_due("token", fake_api, "repo", "usage-keeper/app.db")

        self.assertEqual([item.file_oid for item in fake_api.deleted], ["old"])
        self.assertEqual(len(fake_api.marker_operations), 1)
        self.assertEqual(
            fake_api.marker_operations[0].path_in_repo,
            "usage-keeper/app.manifest.json.lfs-prune.json",
        )


if __name__ == "__main__":
    unittest.main()
