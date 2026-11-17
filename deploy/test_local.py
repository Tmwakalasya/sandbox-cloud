import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("local_setup", Path(__file__).parent / "local/local.py")
local = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(local)


class LocalTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.state = Path(self.temp.name)
        self.scope = patch.object(local, "STATE", self.state)
        self.scope.start()
        self.addCleanup(self.scope.stop)

    def test_kube_explicit_context(self):
        with patch.object(local, "run") as run:
            local.kube("get", "pods")
        self.assertEqual(run.call_args.args[:5],
                         ("kubectl", "--kubeconfig", self.state / "kubeconfig", "--context", local.CONTEXT))

    def test_k3d_side_effects_use_private_kubeconfig(self):
        with patch.object(local.subprocess, "run") as execute:
            local.run("k3d", "cluster", "delete", local.CLUSTER)
        self.assertEqual(execute.call_args.kwargs["env"]["KUBECONFIG"],
                         str(self.state / "kubeconfig"))

    def test_loopback_port_forward(self):
        args = local.port_forward()
        self.assertIn("--address=127.0.0.1", args)
        self.assertIn(str(self.state / "kubeconfig"), args)
        self.assertIn(local.CONTEXT, args)

    def test_registry_name_matches_explicit_k3d_config(self):
        config = (local.HERE / "k3d.yaml").read_text()
        self.assertIn(f"registries:\n  create:\n    name: {local.REGISTRY}\n", config)

    def test_inventory_finds_registry_created_by_config(self):
        values = [json.dumps([{"name": local.CLUSTER}]),
                  json.dumps([{"name": "sandbox-cloud-registry.localhost"}])]
        with patch.object(local, "run", side_effect=values):
            self.assertEqual(local.inventory(), (True, True))

    def test_refuses_adoption(self):
        with patch.object(local, "run", return_value="engine"), \
                patch.object(local, "inventory", return_value=(True, False)):
            with self.assertRaisesRegex(RuntimeError, "refusing to adopt"):
                local.ownership(create=True)
        self.assertFalse((self.state / "owner.json").exists())

    def test_refuses_adoption_of_configured_registry_without_cluster(self):
        values = ["engine", "[]", json.dumps([{"name": "sandbox-cloud-registry.localhost"}])]
        with patch.object(local, "run", side_effect=values):
            with self.assertRaisesRegex(RuntimeError, "refusing to adopt"):
                local.ownership(create=True)
        self.assertFalse((self.state / "owner.json").exists())

    def test_tracks_engine_and_rejects_changed_context(self):
        with patch.object(local, "run", return_value="engine"), \
                patch.object(local, "inventory", return_value=(False, False)):
            local.ownership(create=True)
            local.ownership()
        with patch.object(local, "run", return_value="other"):
            with self.assertRaisesRegex(RuntimeError, "another Docker engine"):
                local.ownership()

    def test_missing_marker_cannot_stop_or_delete(self):
        with patch.object(local, "run", return_value="engine"):
            with self.assertRaisesRegex(RuntimeError, "No local ownership"):
                local.ownership()

    def test_push_digest_rewritten_for_cluster(self):
        for target in ["control", "workspace"]:
            with self.subTest(target=target):
                values = [None, None, json.dumps([f"{local.PUSH}/{target}@sha256:{'a' * 64}"])]
                with patch.object(local, "run", side_effect=values) as run:
                    digest = local.image_digest(target, "unique")
                self.assertEqual(digest, f"sandbox-cloud-registry.localhost:5000/{target}@sha256:{'a' * 64}")
                self.assertEqual(run.call_args_list[0].args[1:3], ("build", "--target"))

    def test_never_deploys_unresolved_digest(self):
        with patch.object(local, "run", side_effect=[None, None, "[]"]):
            with self.assertRaisesRegex(RuntimeError, "Cannot resolve"):
                local.image_digest("control", "unique")

    def test_push_failure_is_not_ignored(self):
        with patch.object(local, "run", side_effect=[None, subprocess.CalledProcessError(1, "push")]):
            with self.assertRaises(subprocess.CalledProcessError):
                local.image_digest("workspace", "unique")

    def test_repeated_up_reuses_cluster_and_zero_pool(self):
        def run(*args, **kwargs):
            if args[:3] == ("k3d", "cluster", "list"):
                return json.dumps([{"name": local.CLUSTER}])
            if args[:3] == ("k3d", "registry", "list"):
                return json.dumps([{"name": "sandbox-cloud-registry.localhost"}])
            if args[:3] == ("k3d", "kubeconfig", "get"):
                return "apiVersion: v1"
            if args[0] == local.sys.executable:
                return "rendered manifest"
        with patch.object(local, "require"), patch.object(local, "ownership"), \
                patch.object(local, "image_digest", return_value="image@sha256:" + "a" * 64), \
                patch.object(local, "run", side_effect=run) as commands, \
                patch.object(local, "kube") as kube:
            local.up()
        args = [call.args for call in commands.call_args_list]
        self.assertIn(("docker", "start", "sandbox-cloud-registry.localhost"), args)
        self.assertIn(("k3d", "cluster", "start", local.CLUSTER), args)
        self.assertFalse(any(call[:3] == ("k3d", "cluster", "create") for call in args))
        renderer = next(call for call in args if call[0] == local.sys.executable)
        self.assertEqual(renderer[-2:], ("--warm-pool-size", "0"))
        self.assertEqual((self.state / "kubeconfig").stat().st_mode & 0o777, 0o600)
        self.assertTrue(kube.called)

    def test_down_requires_confirmation_before_deletion(self):
        with patch.object(local.sys, "argv", ["local.py", "down"]), \
                patch.object(local, "require"), patch.object(local, "ownership"), \
                patch.object(local, "run") as run:
            with self.assertRaises(SystemExit):
                local.main()
        run.assert_not_called()

    def test_down_only_deletes_owned_names(self):
        with patch.object(local.sys, "argv", ["local.py", "down", "--confirm"]), \
                patch.object(local, "require"), patch.object(local, "ownership"), \
                patch.object(local, "inventory", return_value=(True, True)), \
                patch.object(local, "run") as run:
            local.main()
        self.assertEqual([c.args for c in run.call_args_list],
                         [("k3d", "cluster", "delete", local.CLUSTER),
                          ("k3d", "registry", "delete", "sandbox-cloud-registry.localhost")])

    def test_stop_only_stops_owned_names(self):
        with patch.object(local.sys, "argv", ["local.py", "stop"]), \
                patch.object(local, "require"), patch.object(local, "ownership"), \
                patch.object(local, "inventory", return_value=(True, True)), \
                patch.object(local, "run") as run:
            local.main()
        self.assertEqual([c.args for c in run.call_args_list],
                         [("k3d", "cluster", "stop", local.CLUSTER),
                          ("docker", "stop", "sandbox-cloud-registry.localhost")])

    def test_failure_check_verifies_remote_result(self):
        event = json.dumps({"stream": "result", "result": {"state": "failed", "exit_code": 7}})
        error = subprocess.CalledProcessError(7, "sandbox", output=event)
        with patch("builtins.print"), patch.object(local, "run", side_effect=error) as sandbox:
            local.expect_failed_command(sandbox, "workspace", ["sh", "-c", "exit 7"], "failed", 7)

    def test_transport_error_is_not_a_successful_timeout_test(self):
        error = subprocess.CalledProcessError(1, "sandbox", output="")
        with patch.object(local, "run", side_effect=error) as sandbox:
            with self.assertRaisesRegex(RuntimeError, "transport or submission"):
                local.expect_failed_command(sandbox, "workspace", ["sleep", "30"], "timed_out")

    def test_wrong_remote_state_fails_timeout_check(self):
        event = json.dumps({"stream": "result", "result": {"state": "failed", "exit_code": 1}})
        error = subprocess.CalledProcessError(1, "sandbox", output=event)
        with patch.object(local, "run", side_effect=error) as sandbox:
            with self.assertRaises(RuntimeError):
                local.expect_failed_command(sandbox, "workspace", ["sleep", "30"], "timed_out")


if __name__ == "__main__":
    unittest.main()
