import argparse
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from scripts import sync_hf_space


DIGEST = "sha256:" + "a" * 64


class SyncHuggingFaceSpaceTest(unittest.TestCase):
    def test_render_dockerfile_pins_exact_digest(self) -> None:
        rendered = sync_hf_space.render_dockerfile(
            "FROM ghcr.io/willxup/cpa-usage-keeper@__UPSTREAM_IMAGE_DIGEST__\n",
            DIGEST,
        )

        self.assertEqual(
            rendered,
            f"FROM ghcr.io/willxup/cpa-usage-keeper@{DIGEST}\n",
        )

    def test_render_dockerfile_rejects_mutable_or_invalid_digest(self) -> None:
        with self.assertRaisesRegex(ValueError, "invalid upstream image digest"):
            sync_hf_space.render_dockerfile("FROM image@__UPSTREAM_IMAGE_DIGEST__\n", "latest")

    def test_build_state_is_stable_and_records_release(self) -> None:
        state = json.loads(
            sync_hf_space.build_state(
                {
                    "tag_name": "v1.14.5",
                    "html_url": "https://github.com/Willxup/cpa-usage-keeper/releases/tag/v1.14.5",
                    "published_at": "2026-08-18T01:46:16Z",
                },
                DIGEST,
            )
        )

        self.assertEqual(state["tag"], "v1.14.5")
        self.assertEqual(state["manifest_digest"], DIGEST)
        self.assertEqual(state["image"], sync_hf_space.UPSTREAM_IMAGE)

    def test_runtime_stage_reads_huggingface_space_runtime_raw_error(self) -> None:
        runtime = SimpleNamespace(
            stage="PAUSED",
            raw={"errorMessage": "Flagged as abusive"},
        )

        self.assertEqual(
            sync_hf_space.runtime_stage(runtime),
            ("PAUSED", "Flagged as abusive"),
        )

    def test_noop_does_not_create_huggingface_commit(self) -> None:
        release = {
            "tag_name": "v1.14.5",
            "html_url": "https://github.com/Willxup/cpa-usage-keeper/releases/tag/v1.14.5",
            "published_at": "2026-08-18T01:46:16Z",
        }
        with tempfile.TemporaryDirectory() as temp_dir:
            template = Path(temp_dir) / "Dockerfile.template"
            template.write_text("FROM image@__UPSTREAM_IMAGE_DIGEST__\n", encoding="utf-8")
            dockerfile = sync_hf_space.render_dockerfile(template.read_text(encoding="utf-8"), DIGEST)
            state = sync_hf_space.build_state(release, DIGEST)
            api = SimpleNamespace(
                space_info=lambda **_kwargs: SimpleNamespace(sha="space-head"),
                create_commit=lambda **_kwargs: self.fail("no-op must not create a commit"),
            )
            args = argparse.Namespace(
                template=template,
                space=sync_hf_space.SPACE_REPOSITORY,
                dry_run=False,
                verify_timeout_seconds=1,
            )

            with (
                patch.dict("os.environ", {"HF_TOKEN": "test-token"}),
                patch.object(sync_hf_space, "latest_release", return_value=release),
                patch.object(sync_hf_space, "resolve_image_digest", return_value=DIGEST),
                patch.object(sync_hf_space, "HfApi", return_value=api),
                patch.object(sync_hf_space, "remote_text", side_effect=[dockerfile, state]),
            ):
                self.assertEqual(sync_hf_space.sync_space(args), 0)


if __name__ == "__main__":
    unittest.main()
