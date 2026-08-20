import io
import unittest
import urllib.error
from unittest.mock import patch

from scripts import deploy_northflank_usage as deployer


SHA = "1" * 40


class NorthflankUsageDeployerTest(unittest.TestCase):
    def test_deployed_sha_reads_internal_deployment(self) -> None:
        self.assertEqual(
            deployer.deployed_sha({"deployment": {"internal": {"deployedSHA": SHA}}}),
            SHA,
        )

    def test_equal_completed_deployment_is_noop(self) -> None:
        config = deployer.Config()
        with (
            patch.object(deployer, "service_snapshot", return_value={
                "deployment": {"internal": {"deployedSHA": SHA}},
                "status": {"deployment": {"status": "COMPLETED"}},
            }),
            patch.object(deployer, "probe_health") as probe,
        ):
            result = deployer.run(config, "token", SHA, apply=True)
        self.assertEqual(result["action"], "noop")
        probe.assert_called_once_with(config)

    def test_dry_run_never_starts_build(self) -> None:
        config = deployer.Config()
        with (
            patch.object(deployer, "service_snapshot", return_value={
                "deployment": {"internal": {"deployedSHA": "2" * 40}},
                "status": {"deployment": {"status": "COMPLETED"}},
            }),
            patch.object(deployer, "start_build") as start_build,
        ):
            result = deployer.run(config, "token", SHA, apply=False)
        self.assertEqual(result["action"], "would_deploy")
        start_build.assert_not_called()

    def test_start_build_posts_commit_sha(self) -> None:
        config = deployer.Config()
        captured = {}

        def fake_request(method, url, token, payload=None, timeout=30):
            captured.update(method=method, url=url, token=token, payload=payload)
            return {"id": "bright-build-123"}

        with patch.object(deployer, "request_json", side_effect=fake_request):
            self.assertEqual(deployer.start_build(config, "token", SHA), "bright-build-123")
        self.assertEqual(captured["method"], "POST")
        self.assertEqual(captured["payload"], {"sha": SHA})

    def test_health_probe_retries_startup_503(self) -> None:
        config = deployer.Config(timeout_seconds=30, poll_seconds=1)

        class Response:
            status = 200

            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self):
                return b'{"status":"ok"}'

        responses = [
            urllib.error.HTTPError("https://example/healthz", 503, "starting", {}, io.BytesIO()),
            Response(),
        ]
        with patch.object(deployer.urllib.request, "urlopen", side_effect=responses):
            deployer.probe_health(config, sleep=lambda _seconds: None)


if __name__ == "__main__":
    unittest.main()
