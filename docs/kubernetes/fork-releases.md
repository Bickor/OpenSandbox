---
title: Fork releases
description: Publish a complete, digest-pinned AKS demo bundle after every merge.
---

# Fork releases

Every push to `Bickor/OpenSandbox:aks-dev` (including each merged PR) runs
**Release Sandbox Fork**. The release version is `1.1.0-rc.<run-id>.<attempt>`;
the source SHA, immutable image digests, chart, CRDs and checksums are published
together. No path filters skip documentation-only merges. Runs are not cancelled
by a later merge. Manual dispatch and `sandbox-vX.Y.Z-rc.N` candidate tags also work.

The bundle builds server, controller, task-executor, image-committer,
image-committer-azure, execd, ingress and egress from one source commit. The
dashboard is built from the pinned commit of `Bickor/osb-dashboard` recorded in
`manifests/release/dashboard-source.json`. Update that pin to adopt dashboard changes.
The code-server demo image embeds this release's execd payload. All ten images
are published under `ghcr.io/bickor/opensandbox/`.

`release-values.json` pins the active Helm components and server runtime images.
The release includes the chart's CRDs as `crds.yaml`. `release.json` records every
image and source, and `SHA256SUMS` covers every asset. The demo imports the pinned
images into its environment ACR. Adopt later releases through the demo's release
update tool and review the resulting pin change; running clusters never silently
follow a mutable `latest` tag.

Push a new `sandbox-vX.Y.Z-rc.N` tag on the commit containing the desired changes.
The **Release Sandbox Fork** workflow publishes a complete demo-component
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
the release is created only after every image and chart packaging succeed.

The only publishing credential is the scoped GitHub Actions `GITHUB_TOKEN`:
packages write in build jobs and contents write in release assembly. Workflow
inputs never select another source checkout. No Azure credentials are required.
New GHCR packages may default to private: grant consumers package read access or
explicitly make these fork packages public before anonymous AKS/ACR pulls.

## Consume a release

`release.json` records the source SHA, all demo-component image
digests and matching chart checksum. `release-values.json` is a Helm override;
configure the account/container and Workload Identity separately.
`release.json.capabilities.remoteSnapshots` distinguishes the local snapshot
baseline from releases containing Blob support. Only remote-capable releases
include `demo-remote-snapshots.json`, the fragment for the demo Environment's
`spec.platform.remoteSnapshots`. `SHA256SUMS` covers every release asset.

The target is Linux amd64, matching the demo's Kata/MSHV snapshot runtime.
The separately maintained Kata installer retains the demo's existing pin.
An artifact release avoids repeated local builds; it does not establish
cluster compatibility or successful Blob-backed process continuation. Perform
live snapshot/restore validation before promoting a preview for broader use.
