#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
import os
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any, Callable


API_ROOT = "https://api.northflank.com"
DEFAULT_PROJECT = "new-api"
DEFAULT_SERVICE = "daili-usage"
DEFAULT_BASE_URL = "https://p01--daili-usage--q29tm9z7cs9k.code.run"
TERMINAL_BUILD_STATUSES = {"FAILURE", "FAILED", "ERROR", "CANCELLED", "ABORTED"}


class DeployError(RuntimeError):
    pass


@dataclass(frozen=True)
class Config:
    project: str = DEFAULT_PROJECT
    service: str = DEFAULT_SERVICE
    base_url: str = DEFAULT_BASE_URL
    api_root: str = API_ROOT
    timeout_seconds: int = 1800
    poll_seconds: int = 10


def request_json(
    method: str,
    url: str,
    token: str,
    payload: Any = None,
    timeout: int = 30,
) -> dict[str, Any]:
    body = None
    headers = {"Authorization": f"Bearer {token}", "Accept": "application/json"}
    if payload is not None:
        body = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        raise DeployError(f"Northflank {method} {url} returned HTTP {exc.code}: {detail[:1000]}") from exc
    try:
        decoded = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise DeployError(f"Northflank {method} {url} returned invalid JSON") from exc
    if not isinstance(decoded, dict):
        raise DeployError(f"Northflank {method} {url} returned a non-object response")
    if isinstance(decoded.get("error"), dict):
        raise DeployError(f"Northflank {method} {url} returned an error: {decoded['error']}")
    data = decoded.get("data")
    return data if isinstance(data, dict) else decoded


def service_url(config: Config, suffix: str = "") -> str:
    return f"{config.api_root.rstrip('/')}/v1/projects/{config.project}/services/{config.service}{suffix}"


def service_snapshot(config: Config, token: str) -> dict[str, Any]:
    return request_json("GET", service_url(config), token)


def deployed_sha(snapshot: dict[str, Any]) -> str:
    deployment = snapshot.get("deployment")
    if not isinstance(deployment, dict):
        return ""
    internal = deployment.get("internal")
    return str(internal.get("deployedSHA", "")) if isinstance(internal, dict) else ""


def deployment_status(snapshot: dict[str, Any]) -> str:
    status = snapshot.get("status")
    deployment = status.get("deployment") if isinstance(status, dict) else None
    return str(deployment.get("status", "")) if isinstance(deployment, dict) else ""


def build_list(config: Config, token: str) -> list[dict[str, Any]]:
    payload = request_json("GET", service_url(config, "/build"), token)
    builds = payload.get("builds")
    return [item for item in builds if isinstance(item, dict)] if isinstance(builds, list) else []


def start_build(config: Config, token: str, sha: str) -> str:
    payload = request_json("POST", service_url(config, "/build"), token, {"sha": sha})
    for key in ("id", "buildId"):
        value = payload.get(key)
        if isinstance(value, str) and value:
            return value
    builds = build_list(config, token)
    matching = [item for item in builds if str(item.get("sha", "")) == sha]
    if matching and isinstance(matching[0].get("id"), str):
        return str(matching[0]["id"])
    raise DeployError(f"Northflank build response did not include a build id: {payload}")


def wait_for_build(config: Config, token: str, build_id: str, sleep: Callable[[float], None] = time.sleep) -> dict[str, Any]:
    deadline = time.monotonic() + config.timeout_seconds
    while time.monotonic() < deadline:
        builds = build_list(config, token)
        current = next((item for item in builds if item.get("id") == build_id), None)
        if current is None:
            sleep(config.poll_seconds)
            continue
        status = str(current.get("status", ""))
        print(f"Northflank build {build_id}: {status or 'UNKNOWN'}")
        if status == "SUCCESS":
            return current
        if bool(current.get("concluded")) or status in TERMINAL_BUILD_STATUSES:
            raise DeployError(f"Northflank build {build_id} failed: {current}")
        sleep(config.poll_seconds)
    raise DeployError(f"Northflank build {build_id} timed out")


def wait_for_deployment(config: Config, token: str, expected_sha: str, sleep: Callable[[float], None] = time.sleep) -> None:
    deadline = time.monotonic() + config.timeout_seconds
    while time.monotonic() < deadline:
        snapshot = service_snapshot(config, token)
        current_sha = deployed_sha(snapshot)
        status = deployment_status(snapshot)
        print(f"Northflank deployment: sha={current_sha or 'UNKNOWN'} status={status or 'UNKNOWN'}")
        if current_sha == expected_sha and status == "COMPLETED":
            return
        if status in {"ERROR", "FAILED"}:
            raise DeployError(f"Northflank deployment failed: {snapshot}")
        sleep(config.poll_seconds)
    raise DeployError(f"Northflank deployment timed out waiting for {expected_sha}")


def probe_health(config: Config, sleep: Callable[[float], None] = time.sleep) -> None:
    url = config.base_url.rstrip("/") + "/healthz"
    deadline = time.monotonic() + min(config.timeout_seconds, 300)
    last_error = "unavailable"
    while time.monotonic() < deadline:
        request = urllib.request.Request(url, headers={"Accept": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                body = response.read().decode("utf-8", errors="replace")
                if response.status == 200 and '"status":"ok"' in body.replace(" ", ""):
                    print(f"Northflank health probe: HTTP 200 {body[:200]}")
                    return
                last_error = f"HTTP {response.status}: {body[:500]}"
        except urllib.error.HTTPError as exc:
            last_error = f"HTTP {exc.code}"
        except OSError as exc:
            last_error = str(exc)
        print(f"Waiting for Northflank health: {last_error}")
        sleep(config.poll_seconds)
    raise DeployError(f"Northflank health probe timed out: {last_error}")


def run(config: Config, token: str, expected_sha: str, apply: bool) -> dict[str, Any]:
    if not token:
        raise DeployError("NORTHFLANK_API_TOKEN is required")
    if not expected_sha or len(expected_sha) != 40:
        raise DeployError(f"expected SHA must be a full 40-character commit SHA: {expected_sha!r}")
    current = service_snapshot(config, token)
    current_sha = deployed_sha(current)
    current_status = deployment_status(current)
    print(f"Current Northflank deployment: sha={current_sha or 'UNKNOWN'} status={current_status or 'UNKNOWN'}")
    if current_sha == expected_sha and current_status == "COMPLETED":
        if apply:
            probe_health(config)
        return {"action": "noop", "deployed_sha": current_sha, "status": current_status}
    if not apply:
        return {"action": "would_deploy", "current_sha": current_sha, "expected_sha": expected_sha}
    build_id = start_build(config, token, expected_sha)
    build = wait_for_build(config, token, build_id)
    wait_for_deployment(config, token, expected_sha)
    probe_health(config)
    return {"action": "deployed", "build_id": build_id, "build_sha": build.get("sha"), "deployed_sha": expected_sha}


def main() -> int:
    parser = argparse.ArgumentParser(description="Deploy CPA Usage Keeper to the Northflank daili-usage service")
    parser.add_argument("--expected-sha", default=os.environ.get("EXPECTED_SHA", "").strip())
    parser.add_argument("--project", default=os.environ.get("NORTHFLANK_PROJECT", DEFAULT_PROJECT))
    parser.add_argument("--service", default=os.environ.get("NORTHFLANK_SERVICE", DEFAULT_SERVICE))
    parser.add_argument("--base-url", default=os.environ.get("DAILI_USAGE_BASE_URL", DEFAULT_BASE_URL))
    parser.add_argument("--apply", action="store_true")
    parser.add_argument("--timeout-seconds", type=int, default=1800)
    parser.add_argument("--poll-seconds", type=int, default=10)
    args = parser.parse_args()
    config = Config(
        project=args.project,
        service=args.service,
        base_url=args.base_url,
        timeout_seconds=args.timeout_seconds,
        poll_seconds=args.poll_seconds,
    )
    try:
        result = run(config, os.environ.get("NORTHFLANK_API_TOKEN", "").strip(), args.expected_sha, args.apply)
    except Exception as exc:
        print(f"error: {exc}")
        return 1
    print(json.dumps(result, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
