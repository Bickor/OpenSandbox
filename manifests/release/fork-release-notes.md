## AKS sandbox snapshot preview

Versioned **linux/amd64** server, controller and Azure snapshot-worker images,
built together from the tagged source. This includes the Kata VM-state backend
and Azure Blob remote artifact persistence with same-node restoration.

### Assets

- `release.json`: exact source commit, three GHCR image digests and chart SHA-256.
- `release-values.json`: Helm overrides for those exact images.
- `demo-remote-snapshots.json`: Environment `spec.platform.remoteSnapshots` fragment.
- `opensandbox-*.tgz`: matching umbrella chart and CRDs.
- `SHA256SUMS`: checksums for all release assets.

Use digest references from the manifest. Other components (execd, ingress,
egress, task-executor and the separately maintained Kata runtime installer)
retain their existing compatible pins in the demo; this is a snapshot-component
release, not an upstream umbrella release.

This is a preview, **not evidence of successful live AKS/Blob VM restoration**.
The source node, snapshot catalog and restore-plan Secret must still exist.
Cross-node recovery and catalog import are not included. See the tagged
`docs/kubernetes/kata-remote-snapshots.md` and `docs/kubernetes/fork-releases.md`.
