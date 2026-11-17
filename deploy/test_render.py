import pathlib
import subprocess
import sys
import unittest

ROOT = pathlib.Path(__file__).resolve().parent
DIGEST = "a" * 64


class RendererTests(unittest.TestCase):
    def render(self, control=None, workspace=None, *args):
        return subprocess.run(
            [sys.executable, str(ROOT / "render.py"), "--control-image",
             control or f"us-central1-docker.pkg.dev/project/repo/control@sha256:{DIGEST}",
             "--workspace-image", workspace or f"k3d-registry.localhost:5000/workspace@sha256:{DIGEST}",
             *args], text=True, capture_output=True)

    def test_default_preserves_gke_pool(self):
        result = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--warm-pool-size=2", result.stdout)
        self.assertNotIn("CONTROL_IMAGE", result.stdout)
        self.assertIn(f":5000/workspace@sha256:{DIGEST}", result.stdout)

    def test_local_pool_disabled(self):
        result = self.render(None, None, "--warm-pool-size", "0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--warm-pool-size=0", result.stdout)

    def test_pool_boundaries(self):
        self.assertEqual(self.render(None, None, "--warm-pool-size", "16").returncode, 0)
        for size in ["-1", "17", "1.5"]:
            with self.subTest(size=size):
                self.assertNotEqual(self.render(None, None, "--warm-pool-size", size).returncode, 0)

    def test_rejects_mutable_or_injectable_references(self):
        for value in ["control:latest", "repo/control@sha256:abc",
                      f"repo/control@sha256:{'A' * 64}",
                      f"https://host/control@sha256:{DIGEST}",
                      f"host/control@sha256:{DIGEST}\nmetadata: bad",
                      f"host:abc/control@sha256:{DIGEST}"]:
            with self.subTest(value=value):
                self.assertNotEqual(self.render(control=value).returncode, 0)


if __name__ == "__main__":
    unittest.main()
