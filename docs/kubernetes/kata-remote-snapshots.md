---
title: Kata snapshots in Azure Blob Storage
description: Persist Kata snapshot artifacts remotely and prepare a node before VM restore.
---

# Kata snapshots in Azure Blob Storage

The optional Blob backend uploads `kata-vmstate-v1` artifacts before reporting
Ready. Restore downloads and verifies the artifacts before creating the Kata
Pod. Existing snapshot APIs and local-only snapshots remain supported.

Configure the controller's `snapshot.kataVMState.blobAccountURL` and
`blobContainer` Helm values together. Use a private container and configure
the image-committer Pod template with a Workload Identity service account and
the `azure.workload.identity/use: "true"` label. Grant that identity Storage
Blob Data Contributor on the container. No storage credentials enter the sandbox.

If role assignments cannot be created, an operator who already has Blob data
access can supply a short-lived **user-delegation SAS** via the image-committer
Pod template's `KATA_SNAPSHOT_BLOB_SAS_TOKEN` environment variable, sourced from a
Kubernetes Secret. It must be HTTPS-only, container-scoped, and expire within
24 hours; allow read/create/write/list/delete for the snapshot lifecycle. This
optional path does not accept account keys or account SAS. Refresh the Secret
before expiry; existing Jobs retain their original environment and may need to
be retried. The default remains Azure Identity/Workload Identity. SAS transport
errors are redacted because SDK error messages may otherwise contain query
credentials. This path validates storage transport, not Workload Identity RBAC.

For the umbrella chart, these keys are under
`opensandbox-controller.controller.snapshot.kataVMState`. Deploy the controller,
server and image-committer built from the same remote-snapshot commit. The chart
accepts `controller.image.digest` and `server.image.digest` for immutable image
selection; `snapshot.imageCommitterImage` accepts a full digest-qualified image.

The manifest is published last, contains SHA-256 digests and logical file sizes,
and is itself pinned by digest in snapshot status. Files are streamed with zstd
compression; zero-filled regions are restored as sparse files. Transfers use
bounded memory, SDK retries, and the worker Job deadline. The captured Pod
template is uploaded privately alongside the artifacts. Download paths are
validated; links and special files are rejected.

Restore preparation runs in a separate trusted Job on the source node, never in
an init container of the Kata Pod. Concurrent prepares use a node-local lock;
completed caches are verified and incomplete downloads are never published.
The preparation Job remains owned by the restored BatchSandbox until deletion.

## Scope and recovery

This first version retains **same-node placement** and `durable: false` in the
public restore constraints: artifact persistence does not yet guarantee recovery
after losing the node or cluster catalog. The original node must remain Ready.
The Blob package survives local artifact loss, but the catalog and immutable
restore-plan Secret must still exist. Cross-node compatibility and catalog
import are separate milestones. Do not point blob lifecycle deletion policies
at live snapshots. Delete snapshots through OpenSandbox after releasing every
restored sandbox; this removes remote artifacts as well as the local cache.

For validation, create a snapshot, release its source sandbox, remove only its
unused node-local cache, and restore by ID. Verify a marker file and an in-memory
counter to distinguish resumed processes from cold startup. Also test interrupted
uploads, corrupted downloads, simultaneous restores, and delete conflicts.

References: [Blob SDK for Go](https://learn.microsoft.com/azure/storage/blobs/storage-blob-go-get-started)
and [AKS Workload Identity](https://learn.microsoft.com/azure/aks/workload-identity-overview).
