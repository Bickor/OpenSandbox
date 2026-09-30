"""Assemble a complete, source-consistent AKS demo release."""

import argparse
import hashlib
import json
import re
from pathlib import Path

COMPONENTS = {"controller", "server", "image-committer-azure", "image-committer",
              "task-executor", "execd", "ingress", "egress", "osb-dashboard", "code-server-execd"}


def assemble(metadata: Path, output: Path, version: str, commit: str, repository: str, remote_snapshots: bool = False):
    if not re.fullmatch(r"\d+\.\d+\.\d+-rc\.\d+(?:\.\d+)?", version):
        raise ValueError("expected X.Y.Z-rc.N preview version")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("expected full source commit SHA")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("invalid repository")
    images = {}
    for path in sorted(metadata.glob("*.json")):
        item = json.loads(path.read_text())
        component = item["component"]
        if component not in COMPONENTS or component in images:
            raise ValueError("unexpected or duplicate image component")
        if item["commit"] != commit or item["platform"] != "linux/amd64":
            raise ValueError("inconsistent source commit or platform")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", item["digest"]):
            raise ValueError("invalid image digest")
        expected = f"ghcr.io/{repository.split('/')[0].lower()}/opensandbox/{component}@{item['digest']}"
        if item["image"] != expected:
            raise ValueError("image does not belong to release repository")
        images[component] = item["image"]
    if set(images) != COMPONENTS:
        raise ValueError("release requires all demo components")
    chart = output / f"opensandbox-{version}.tgz"
    chart_digest = hashlib.sha256(chart.read_bytes()).hexdigest()

    def image(component):
        repo, digest = images[component].split("@")
        return {"repository": repo, "digest": digest}

    assets = {
        "release.json": {
            "schemaVersion": 1,
            "tag": f"sandbox-v{version}",
            "repository": repository,
            "commit": commit,
            "platforms": ["linux/amd64"],
            "capabilities": {"kataVMState": True, "remoteSnapshots": remote_snapshots, "restorePlacement": "same-node"},
            "images": images,
            "sources": {"osb-dashboard": json.loads((Path(__file__).parent / "dashboard-source.json").read_text())},
            "chart": {"file": chart.name, "sha256": chart_digest},
        },
        "release-values.json": {
            "opensandbox-controller": {"controller": {
                "image": image("controller"),
                "snapshot": {"imageCommitterImage": images["image-committer-azure"], "kataVMState": {"enabled": True}},
            }},
            "opensandbox-server": {
                "server": {"image": image("server")},
                "configToml": (
                    '[server]\nhost = "0.0.0.0"\nport = 80\napi_key = ""\n\n'
                    '[runtime]\ntype = "kubernetes"\nexecd_image = "' + images["execd"] + '"\n\n'
                    '[kubernetes]\nnamespace = "opensandbox"\nworkload_provider = "batchsandbox"\n\n'
                    '[egress]\nimage = "' + images["egress"] + '"\nmode = "dns+nft"\n'
                ),
            },
            "ingress-gateway": {"gateway": {"image": image("ingress")}},
            "opensandbox-node-agent": {"enabled": False},
        },
    }
    if remote_snapshots:
        assets["demo-remote-snapshots.json"] = {
            "enabled": True, "controllerImage": images["controller"],
            "serverImage": images["server"], "committerImage": images["image-committer-azure"],
        }
    for name, content in assets.items():
        (output / name).write_text(json.dumps(content, indent=2, sort_keys=True) + "\n")
    crds = output / "crds.yaml"
    if not crds.is_file():
        raise ValueError("release requires chart CRDs")
    files = [chart, crds, *(output / name for name in assets)]
    (output / "SHA256SUMS").write_text("".join(
        f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n" for path in sorted(files)
    ))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--metadata", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--remote-snapshots", action="store_true")
    assemble(**vars(parser.parse_args()))
