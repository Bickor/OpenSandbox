import json
import tempfile
import unittest
from pathlib import Path

from fork_release import assemble, COMPONENTS


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.metadata = self.root / "metadata"
        self.output = self.root / "output"
        self.metadata.mkdir()
        self.output.mkdir()
        self.version = "0.1.0-rc.1"
        self.commit = "a" * 40
        (self.output / f"opensandbox-{self.version}.tgz").write_bytes(b"chart")
        for component in COMPONENTS:
            self.write(component)

    def write(self, component, **overrides):
        value = {"component": component, "commit": self.commit, "platform": "linux/amd64",
                 "digest": "sha256:" + "b" * 64,
                 "image": f"ghcr.io/bickor/opensandbox/{component}@sha256:" + "b" * 64}
        value.update(overrides)
        (self.metadata / f"{component}.json").write_text(json.dumps(value))

    def assemble(self, remote_snapshots=False):
        assemble(self.metadata, self.output, self.version, self.commit, "Bickor/OpenSandbox", remote_snapshots)

    def test_complete_bundle(self):
        self.assemble()
        release = json.loads((self.output / "release.json").read_text())
        self.assertEqual(set(release["images"]), COMPONENTS)
        self.assertEqual(release["commit"], self.commit)
        self.assertEqual(len((self.output / "SHA256SUMS").read_text().splitlines()), 3)
        self.assertFalse(release["capabilities"]["remoteSnapshots"])
        self.assertFalse((self.output / "demo-remote-snapshots.json").exists())
        values = json.loads((self.output / "release-values.json").read_text())
        self.assertEqual(values["opensandbox-server"]["server"]["image"]["digest"], "sha256:" + "b" * 64)

    def test_remote_bundle(self):
        self.assemble(remote_snapshots=True)
        self.assertTrue(json.loads((self.output / "release.json").read_text())["capabilities"]["remoteSnapshots"])
        self.assertTrue((self.output / "demo-remote-snapshots.json").exists())
        self.assertEqual(len((self.output / "SHA256SUMS").read_text().splitlines()), 4)

    def test_qemu_bundle(self):
        assemble(self.metadata, self.output, self.version, self.commit, "Bickor/OpenSandbox", qemu_snapshots=True)
        capabilities = json.loads((self.output / "release.json").read_text())["capabilities"]
        self.assertTrue(capabilities["qemuSnapshots"])
        self.assertFalse(capabilities["remoteSnapshots"])

    def test_rejects_mixed_commits(self):
        self.write("server", commit="c" * 40)
        with self.assertRaises(ValueError):
            self.assemble()

    def test_rejects_missing_component(self):
        (self.metadata / "server.json").unlink()
        with self.assertRaises(ValueError):
            self.assemble()

    def test_rejects_foreign_registry(self):
        self.write("server", image="ghcr.io/other/server@sha256:" + "b" * 64)
        with self.assertRaises(ValueError):
            self.assemble()

    def test_rejects_invalid_version(self):
        self.version = "../../escape"
        with self.assertRaises(ValueError):
            self.assemble()
