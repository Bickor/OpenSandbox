---
title: UID-bound pool allocation
description: Opt-in immutable allocation identities for single-use pool Pods.
---

# UID-bound pool allocation (prerequisite only)

`uid-bound-v1` is an opt-in allocation identity contract, **not network policy
enforcement**. It creates no NetworkPolicy, authorization objects, or runtime
injection. Markerless native allocation, including legacy pods-only
`alloc-status` JSON, is unchanged.

## Required trust boundary

Before using this mode, install namespace-scoped admission and a trusted,
operator-configured lifecycle backend. Both Pool and pooled BatchSandbox must
carry `sandbox.opensandbox.io/allocation-mode: uid-bound-v1` **from creation**.
Admission must require and immutably retain these markers, forbid adoption of
existing unbound resources, and protect the controller-owned allocation
annotations and Pod reservations from caller modification. The controller
cannot detect removal of all evidence and does not replace this admission
boundary. Do not enable this mode on an existing native Pool or allocation.

Only one replica per BatchSandbox and `recycleStrategy.type: Delete` (including
the default Delete strategy) are supported. Used Pods are deleted, never reset
or returned for reuse; normal Pool scaling replenishes capacity. Pause/resume
and pool auto-assignment are outside this initial contract.

## Annotation contract

The Pool and BatchSandbox mode is exactly
`sandbox.opensandbox.io/allocation-mode: uid-bound-v1`. Pool-created Pods inherit
that marker. The controller constructs the following identity from the exact
selected objects, not from later name resolution:

```json
{
  "version": "uid-bound-v1",
  "batchSandbox": {"name": "sandbox-a", "uid": "batch-uid"},
  "pool": {"name": "pool-a", "uid": "pool-uid"},
  "pod": {"name": "pool-a-abc", "uid": "pod-uid"}
}
```

This JSON is stored under `sandbox.opensandbox.io/alloc-identity`. All objects
must be in the same namespace. Names and UIDs are required. First the controller
fences an immutable `sandbox.opensandbox.io/alloc-intent` annotation on the
BatchSandbox, containing the same JSON and retained permanently. Admission must
also protect this controller-owned intent. It preserves the original selection
even if the Pod disappears before publication, and is not a usable allocation.
The same identity is then persisted on the selected Pod as a reservation, then on the
BatchSandbox **atomically with** its unchanged `alloc-status` shape:
`{"pods":["pool-a-abc"],"poolRef":"pool-a","generation":1}`. Publication uses UID
and resourceVersion fencing. `generation` is not a freshness predicate.

Kubernetes does not offer a transaction across these three resources. A
concurrent deletion may leave an unusable binding, but can never change its
UIDs. Every protected consumer must revalidate live identities; an allocation
annotation alone is insufficient evidence.

A failed or ambiguous publication leaves the Pod reservation intact. Retries
and controller restarts recover that same pin; they cannot select another Pod.
An orphan reservation is deleted, not reassigned. Existing identities are never
upgraded or rebound. A marked BatchSandbox with any previous name-only
allocation is rejected, rather than backfilled.

Readers require the complete tuple to match across Batch intent, Batch identity,
and Pod reservation. They reject unknown modes/versions, missing or corrupt
intents or bindings, a binding without its mode marker, mismatched allocation names, and replaced BatchSandbox,
Pool, or Pod UIDs. Scheduling and endpoint resolution use live objects to
validate the original UIDs. The pure `pkg/utils.GetEndpoints` helper refuses
protected allocations; callers must use `GetEndpointsWithReader` with an
uncached reader. Endpoint annotations alone are not protected identity evidence.

The public constants, schema, parser, and validation helpers live in
`kubernetes/pkg/utils/allocation_identity.go` for downstream consumers to mirror.
