# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""
Helpers for resolving sandbox create requests from snapshots.
"""

from __future__ import annotations

from fastapi import HTTPException, status
from starlette.concurrency import run_in_threadpool

from opensandbox_server.api.schema import CreateSandboxRequest, ImageSpec
from opensandbox_server.constants import OPENSANDBOX_LIFECYCLE
from opensandbox_server.repositories.snapshots.factory import get_snapshot_repository
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.snapshot_models import SnapshotRestoreConfig, SnapshotState
from opensandbox_server.tenants.context import get_current_tenant

DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT = ["tail", "-f", "/dev/null"]
KATA_VMSTATE_FORMAT = "kata-vmstate-v1"


def _reject_kata_restore_conflicts(request: CreateSandboxRequest) -> None:
    public_names = {
        "snapshot_id": "snapshotId",
        "resource_limits": "resourceLimits",
        "resource_requests": "resourceRequests",
        "network_policy": "networkPolicy",
        "credential_proxy": "credentialProxy",
        "secure_access": "secureAccess",
        "template_id": "templateId",
    }
    allowed = {"snapshot_id", "timeout", "metadata"}
    present = sorted(
        public_names.get(name, name)
        for name in request.model_fields_set
        if name not in allowed
    )
    if present:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    "kata-vmstate-v1 snapshot restore accepts only snapshotId, timeout, "
                    f"and metadata; conflicting fields: {', '.join(present)}."
                ),
            },
        )


async def resolve_sandbox_image_from_request(
    request: CreateSandboxRequest,
) -> CreateSandboxRequest:
    """
    Normalize a sandbox create request to an effective image-backed request.

    When `snapshotId` is used, this resolves the snapshot from server
    persistence and injects `request.image` from `restore_config.image`, and
    records the owning backend so composite routing can dispatch the create.
    Requests without a snapshotId (image-backed, pool-only, template-mode)
    pass through unchanged: image-or-snapshotId validity is the schema
    validator's job.
    """

    if not (request.snapshot_id or "").strip():
        # Image-backed and pool-only (extensions.poolRef) creates have no
        # snapshot to resolve; the schema validator owns their validation.
        return request

    snapshot_id = (request.snapshot_id or "").strip()
    if request.snapshot_restore_config is not None:
        return request

    snapshot_repository = get_snapshot_repository()
    snapshot = await run_in_threadpool(snapshot_repository.get, snapshot_id)
    if snapshot is None:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail={
                "code": "SNAPSHOT::NOT_FOUND",
                "message": f"Snapshot {snapshot_id} not found",
            },
        )

    tenant = get_current_tenant()
    if tenant is not None and (snapshot.namespace is None or snapshot.namespace != tenant.namespace):
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail={
                "code": "SNAPSHOT::NOT_FOUND",
                "message": f"Snapshot {snapshot_id} not found",
            },
        )

    if snapshot.status.state != SnapshotState.READY:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "code": "SNAPSHOT::NOT_READY",
                "message": f"Snapshot {snapshot_id} is not ready for restore.",
            },
        )

    if snapshot.restore_config.format == KATA_VMSTATE_FORMAT:
        if not snapshot.restore_config.is_complete_kata_plan():
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": "SNAPSHOT::INVALID_RESTORE_CONFIG",
                    "message": f"Snapshot {snapshot_id} does not have a complete Kata restore plan.",
                },
            )
        _reject_kata_restore_conflicts(request)
        request.snapshot_id = snapshot_id
        request._resolved_snapshot_backend = snapshot.restore_config.backend
        request._snapshot_restore_config = SnapshotRestoreConfig.from_dict(
            snapshot.restore_config.to_dict()
        )
        return request

    if request.image is not None or request.template_id is not None:
        field = "image" if request.image is not None else "templateId"
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": f"{field} cannot be combined with snapshotId.",
            },
        )

    if (request.extensions or {}).get("poolRef", "").strip():
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": "snapshotId cannot be used together with poolRef.",
            },
        )
    if request.env and OPENSANDBOX_LIFECYCLE in request.env:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f"Environment variable {OPENSANDBOX_LIFECYCLE!r} is reserved. "
                    "Use the lifecycle request field instead."
                ),
            },
        )
    if (
        request.credential_proxy
        and request.credential_proxy.enabled
        and request.network_policy is None
    ):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": "credentialProxy.enabled requires networkPolicy.",
            },
        )

    if request.resource_limits is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": "resourceLimits is required for image-backed snapshot restore.",
            },
        )

    restore_image = (snapshot.restore_config.image or "").strip()
    if not restore_image:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "code": "SNAPSHOT::INVALID_RESTORE_CONFIG",
                "message": f"Snapshot {snapshot_id} does not have a restorable image.",
            },
        )

    request.image = ImageSpec(uri=restore_image, auth=None)
    request.snapshot_id = snapshot_id
    request._resolved_snapshot_backend = (snapshot.restore_config.backend or "").strip() or None
    request._snapshot_restore_config = SnapshotRestoreConfig.from_dict(
        snapshot.restore_config.to_dict()
    )
    if not request.entrypoint:
        request.entrypoint = list(DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT)
    return request


async def resolve_sandbox_from_request(
    request: CreateSandboxRequest,
) -> CreateSandboxRequest:
    """Resolve a snapshot-backed create request to its effective restore plan."""
    return await resolve_sandbox_image_from_request(request)


async def verify_snapshot_restore_ready(request: CreateSandboxRequest) -> None:
    """Revalidate a resolved restore after its zero-replica consumer reservation exists."""
    snapshot_id = (request.snapshot_id or "").strip()
    if not snapshot_id or request.snapshot_restore_config is None:
        return
    snapshot = await run_in_threadpool(get_snapshot_repository().get, snapshot_id)
    if snapshot is None or snapshot.status.state != SnapshotState.READY:
        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "code": "SNAPSHOT::NOT_READY",
                "message": f"Snapshot {snapshot_id} is no longer ready for restore.",
            },
        )


__all__ = [
    "DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT",
    "resolve_sandbox_from_request",
    "resolve_sandbox_image_from_request",
    "verify_snapshot_restore_ready",
]
