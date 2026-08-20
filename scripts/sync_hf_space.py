#!/usr/bin/env python3

import argparse
import json
import os
import re
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Dict, Optional, Tuple

from huggingface_hub import CommitOperationAdd, HfApi, hf_hub_download
from huggingface_hub.errors import EntryNotFoundError


UPSTREAM_REPOSITORY = "Willxup/cpa-usage-keeper"
UPSTREAM_IMAGE = "ghcr.io/willxup/cpa-usage-keeper"
SPACE_REPOSITORY = "pjpjq/daili-usage-keeper"
REPOSITORY_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_TEMPLATE = REPOSITORY_ROOT / "deploy/huggingface/Dockerfile.template"
DIGEST_PATTERN = re.compile(r"sha256:[0-9a-f]{64}\Z")
TERMINAL_FAILURE_STAGES = {"BUILD_ERROR", "CONFIG_ERROR", "RUNTIME_ERROR"}
BUILDING_STAGES = {
    "BUILDING",
    "RUNNING_BUILDING",
    "RUNNING_APP_STARTING",
    "RUNNING_STARTING",
}


def request_json(url: str, headers: Optional[Dict[str, str]] = None) -> Tuple[Any, Dict[str, str]]:
    request = urllib.request.Request(url, headers=headers or {})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response), dict(response.headers.items())


def request_headers(url: str, headers: Optional[Dict[str, str]] = None) -> Dict[str, str]:
    request = urllib.request.Request(url, headers=headers or {}, method="HEAD")
    with urllib.request.urlopen(request, timeout=30) as response:
        return dict(response.headers.items())


def latest_release() -> Dict[str, Any]:
    payload, _ = request_json(
        f"https://api.github.com/repos/{UPSTREAM_REPOSITORY}/releases/latest",
        headers={
            "Accept": "application/vnd.github+json",
            "User-Agent": "daili-usage-keeper-hf-updater",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    tag = str(payload.get("tag_name", "")).strip()
    if not tag or payload.get("draft") or payload.get("prerelease"):
        raise RuntimeError("latest GitHub release is not a stable tagged release")
    return payload


def resolve_image_digest(tag: str, attempts: int = 4) -> str:
    token_url = "https://ghcr.io/token?" + urllib.parse.urlencode(
        {
            "scope": "repository:willxup/cpa-usage-keeper:pull",
            "service": "ghcr.io",
        }
    )
    last_error: Optional[Exception] = None
    for attempt in range(attempts):
        try:
            token_payload, _ = request_json(token_url)
            registry_token = str(token_payload.get("token", "")).strip()
            if not registry_token:
                raise RuntimeError("GHCR token response did not include a token")
            manifest_url = f"https://ghcr.io/v2/willxup/cpa-usage-keeper/manifests/{urllib.parse.quote(tag, safe='')}"
            headers = request_headers(
                manifest_url,
                headers={
                    "Accept": ", ".join(
                        [
                            "application/vnd.oci.image.index.v1+json",
                            "application/vnd.docker.distribution.manifest.list.v2+json",
                            "application/vnd.oci.image.manifest.v1+json",
                            "application/vnd.docker.distribution.manifest.v2+json",
                        ]
                    ),
                    "Authorization": f"Bearer {registry_token}",
                    "User-Agent": "daili-usage-keeper-hf-updater",
                },
            )
            digest = next(
                (value for key, value in headers.items() if key.lower() == "docker-content-digest"),
                "",
            ).strip()
            if not DIGEST_PATTERN.fullmatch(digest):
                raise RuntimeError(f"GHCR returned an invalid manifest digest: {digest!r}")
            return digest
        except Exception as exc:
            last_error = exc
            if attempt + 1 < attempts:
                time.sleep(2 ** attempt)
    raise RuntimeError(f"unable to resolve GHCR digest for {tag}: {last_error}")


def render_dockerfile(template: str, digest: str) -> str:
    if not DIGEST_PATTERN.fullmatch(digest):
        raise ValueError(f"invalid upstream image digest: {digest!r}")
    placeholder = "__UPSTREAM_IMAGE_DIGEST__"
    if template.count(placeholder) != 1:
        raise ValueError("Dockerfile template must contain exactly one digest placeholder")
    return template.replace(placeholder, digest)


def build_state(release: Dict[str, Any], digest: str) -> str:
    payload = {
        "image": UPSTREAM_IMAGE,
        "manifest_digest": digest,
        "release_url": release.get("html_url"),
        "published_at": release.get("published_at"),
        "tag": release["tag_name"],
    }
    return json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def remote_text(repo_id: str, path: str, revision: str, token: str) -> Optional[str]:
    try:
        downloaded = hf_hub_download(
            repo_id=repo_id,
            repo_type="space",
            filename=path,
            revision=revision,
            token=token,
        )
    except EntryNotFoundError:
        return None
    return Path(downloaded).read_text(encoding="utf-8")


def runtime_stage(runtime: Any) -> Tuple[str, str]:
    if isinstance(runtime, dict):
        return str(runtime.get("stage", "")), str(runtime.get("errorMessage", ""))
    raw = getattr(runtime, "raw", None)
    if not isinstance(raw, dict):
        raw = {}
    stage = str(getattr(runtime, "stage", "") or raw.get("stage", ""))
    error_message = str(
        getattr(runtime, "error_message", "")
        or raw.get("errorMessage", "")
        or raw.get("error_message", "")
    )
    return stage, error_message


def verify_runtime(api: HfApi, repo_id: str, token: str, expected_sha: str, timeout_seconds: int) -> None:
    deadline = time.monotonic() + timeout_seconds
    last_stage = ""
    while True:
        info = api.space_info(repo_id=repo_id, token=token)
        current_sha = str(getattr(info, "sha", ""))
        stage, error_message = runtime_stage(getattr(info, "runtime", None))
        if current_sha != expected_sha:
            if time.monotonic() >= deadline:
                raise RuntimeError(f"Space head did not reach committed SHA {expected_sha}; current={current_sha}")
        elif stage in TERMINAL_FAILURE_STAGES:
            raise RuntimeError(f"Space entered {stage}: {error_message}")
        elif stage == "RUNNING":
            print(f"Space runtime verified: sha={current_sha} stage={stage}")
            return
        elif stage == "PAUSED":
            if "Quota exceeded" in error_message:
                print(
                    f"::warning::Space source updated to {current_sha}, but runtime verification is blocked by quota: {error_message}"
                )
                return
            raise RuntimeError(f"Space is PAUSED after update: {error_message}")
        elif stage and stage not in BUILDING_STAGES and time.monotonic() >= deadline:
            raise RuntimeError(f"Space did not become RUNNING; stage={stage} error={error_message}")
        if stage != last_stage:
            print(f"Waiting for Space runtime: sha={current_sha} stage={stage or 'UNKNOWN'}")
            last_stage = stage
        if time.monotonic() >= deadline:
            raise RuntimeError(f"Space verification timed out; sha={current_sha} stage={stage} error={error_message}")
        time.sleep(15)


def sync_space(args: argparse.Namespace) -> int:
    token = os.environ.get("HF_TOKEN", "").strip()
    if not token:
        raise RuntimeError("HF_TOKEN is required")
    release = latest_release()
    tag = str(release["tag_name"])
    digest = resolve_image_digest(tag)
    template = args.template.read_text(encoding="utf-8")
    dockerfile = render_dockerfile(template, digest)
    state = build_state(release, digest)
    api = HfApi(token=token)
    info = api.space_info(repo_id=args.space, token=token)
    parent_sha = str(getattr(info, "sha", ""))
    if not parent_sha:
        raise RuntimeError("Hugging Face Space did not return a head SHA")
    current_dockerfile = remote_text(args.space, "Dockerfile", parent_sha, token)
    current_state = remote_text(args.space, ".upstream-version.json", parent_sha, token)
    print(f"Latest upstream release: {tag}")
    print(f"Immutable upstream image: {UPSTREAM_IMAGE}@{digest}")
    print(f"Current Space head: {parent_sha}")
    if current_dockerfile == dockerfile and current_state == state:
        print("Space already uses the latest immutable upstream image; no update needed")
        return 0
    if args.dry_run:
        print("Dry run: Space would be updated")
        return 0
    commit = api.create_commit(
        repo_id=args.space,
        repo_type="space",
        revision="main",
        parent_commit=parent_sha,
        token=token,
        commit_message=f"chore: update CPA Usage Keeper to {tag}",
        operations=[
            CommitOperationAdd(path_in_repo="Dockerfile", path_or_fileobj=dockerfile.encode("utf-8")),
            CommitOperationAdd(path_in_repo=".upstream-version.json", path_or_fileobj=state.encode("utf-8")),
        ],
    )
    commit_sha = str(getattr(commit, "oid", "") or getattr(commit, "commit_id", ""))
    if not commit_sha:
        raise RuntimeError("Hugging Face commit response did not include a commit SHA")
    print(f"Updated Space source: {commit_sha}")
    verify_runtime(api, args.space, token, commit_sha, args.verify_timeout_seconds)
    return 0


def parse_args(argv: Optional[list] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Update the CPA Usage Keeper Hugging Face Space")
    parser.add_argument(
        "--template",
        type=Path,
        default=DEFAULT_TEMPLATE,
        help="overlay Dockerfile template",
    )
    parser.add_argument("--space", default=SPACE_REPOSITORY, help="target Hugging Face Space repository")
    parser.add_argument("--dry-run", action="store_true", help="resolve and compare without committing")
    parser.add_argument("--verify-timeout-seconds", type=int, default=1200)
    return parser.parse_args(argv)


def main() -> int:
    try:
        return sync_space(parse_args())
    except Exception as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
