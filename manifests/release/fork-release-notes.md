## Complete AKS demo fork bundle

Linux/amd64 images built for this source commit: server, controller,
task-executor, image-committer, image-committer-azure, execd, ingress, egress,
the pinned Bickor/osb-dashboard source, and code-server with this release's execd.

- `release.json`: source commits, exact GHCR digests, capabilities and chart hash.
- `release-values.json`: digest-pinned Helm values for active components.
- `opensandbox-*.tgz`: matching umbrella chart and CRDs.
- `crds.yaml`: chart CRDs for explicit installation.
- `demo-remote-snapshots.json`: remote snapshot image overrides.
- `SHA256SUMS`: checksums for every release asset.

Use the digest references from the manifest. The Kata runtime installer is
maintained separately. Artifact publication does not establish live cluster
validation; see the consuming demo PR for test results.
