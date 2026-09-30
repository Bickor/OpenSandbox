---
title: Fork snapshot releases
description: Build once in GitHub Actions and consume versioned snapshot components.
---

# Fork snapshot releases

Push a new `sandbox-vX.Y.Z-rc.N` tag on the commit containing the desired changes.
The **Release Sandbox Fork** workflow publishes a complete snapshot-component
preview to the fork's own GHCR namespace and creates a GitHub prerelease.
The tag must contain this workflow. No default-branch or upstream merge is
required for tag-triggered releases. Keep draft PRs open for code review.

```bash
git tag sandbox-v0.1.0-rc.2 <reviewed-commit>
git push personal-fork sandbox-v0.1.0-rc.2
gh run list --repo Bickor/OpenSandbox --workflow release-sandbox-fork.yml
gh release download sandbox-v0.1.0-rc.2 --repo Bickor/OpenSandbox --dir release
```

Always use a fresh version for changed source. The workflow refuses to overwrite
an existing release; image candidate tags include run and attempt IDs. Consumers
use image digests, not mutable tags. Failed unpublished releases can be retried;
the release is created only after all three images and chart packaging succeed.

The only publishing credential is the scoped GitHub Actions `GITHUB_TOKEN`:
packages write in build jobs and contents write in release assembly. Workflow
inputs never select another source checkout. No Azure credentials are required.
New GHCR packages may default to private: grant consumers package read access or
explicitly make these fork packages public before anonymous AKS/ACR pulls.

## Consume a release

QEMU-capable releases declare `capabilities.qemuSnapshots: true`. Their Azure
committer uses `kubernetes/Dockerfile.image-committer-azure`, including nerdctl,
the standard QEMU worker, checkpoint helper and VMState loader. The release job
checks those executables in the published image before publishing the bundle.
Consumers should verify the source commit includes the public QEMU restore
changes as well as checking the capability and immutable image/chart digests.

`release.json` records the source SHA, controller/server/Azure-committer image
digests and matching chart checksum. `release-values.json` is a Helm override;
configure the account/container and Workload Identity separately.
`release.json.capabilities.remoteSnapshots` distinguishes the local snapshot
baseline from releases containing Blob support. Only remote-capable releases
include `demo-remote-snapshots.json`, the fragment for the demo Environment's
`spec.platform.remoteSnapshots`. `SHA256SUMS` covers every release asset.

The target is Linux amd64, matching the demo's Kata/MSHV snapshot runtime.
Other OpenSandbox components and the Kata installer retain the demo's existing
pins. An artifact release avoids repeated local builds; it does not establish
cluster compatibility or successful Blob-backed process continuation. Perform
live snapshot/restore validation before promoting a preview for broader use.
