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

from datetime import datetime, timezone

import pytest
from fastapi import HTTPException

from opensandbox_server.api.schema import CreateSandboxRequest, ImageSpec, ResourceLimits
from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository
from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)
from opensandbox_server.services.snapshot_restore import (
    DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT,
    resolve_sandbox_image_from_request,
)


@pytest.mark.asyncio
async def test_snapshot_restore_resolves_effective_image(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-001",
            source_sandbox_id="sbx-001",
            restore_config=SnapshotRestoreConfig(image="registry.example.com/snapshots/snap-001:latest"),
            status=SnapshotStatusRecord(
                state=SnapshotState.READY,
                last_transition_at=datetime.now(timezone.utc),
            ),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.get_snapshot_repository",
        lambda: repo,
    )

    request = CreateSandboxRequest(
        snapshotId="snap-001",
        resourceLimits=ResourceLimits(root={"cpu": "500m"}),
    )

    resolved = await resolve_sandbox_image_from_request(request)
    assert resolved.image is not None
    assert resolved.image.uri == "registry.example.com/snapshots/snap-001:latest"
    assert resolved.snapshot_id == "snap-001"
    assert resolved.entrypoint == DEFAULT_SNAPSHOT_RESTORE_ENTRYPOINT
    assert resolved.resolved_snapshot_backend is None


@pytest.mark.asyncio
async def test_snapshot_restore_records_fsb_backend_hint(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-fsb-001",
            source_sandbox_id="fsb-001",
            restore_config=SnapshotRestoreConfig(
                image="registry.example.com/fsb/snap-001:index",
                backend="fsb",
            ),
            status=SnapshotStatusRecord(
                state=SnapshotState.READY,
                last_transition_at=datetime.now(timezone.utc),
            ),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.get_snapshot_repository",
        lambda: repo,
    )

    request = CreateSandboxRequest(
        snapshotId="snap-fsb-001",
        resourceLimits=ResourceLimits(root={"cpu": "500m"}),
    )

    resolved = await resolve_sandbox_image_from_request(request)
    assert resolved.image is not None
    assert resolved.image.uri == "registry.example.com/fsb/snap-001:index"
    assert resolved.resolved_snapshot_backend == "fsb"


@pytest.mark.asyncio
async def test_snapshot_restore_preserves_explicit_entrypoint(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-003",
            source_sandbox_id="sbx-001",
            restore_config=SnapshotRestoreConfig(image="registry.example.com/snapshots/snap-003:latest"),
            status=SnapshotStatusRecord(
                state=SnapshotState.READY,
                last_transition_at=datetime.now(timezone.utc),
            ),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.get_snapshot_repository",
        lambda: repo,
    )

    request = CreateSandboxRequest(
        snapshotId="snap-003",
        resourceLimits=ResourceLimits(root={"cpu": "500m"}),
        entrypoint=["python", "app.py"],
    )

    resolved = await resolve_sandbox_image_from_request(request)
    assert resolved.image is not None
    assert resolved.image.uri == "registry.example.com/snapshots/snap-003:latest"
    assert resolved.snapshot_id == "snap-003"
    assert resolved.entrypoint == ["python", "app.py"]


@pytest.mark.asyncio
async def test_image_snapshot_restore_requires_resource_limits(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-image",
            source_sandbox_id="sbx-001",
            restore_config=SnapshotRestoreConfig(image="registry.example.com/snapshot:1"),
            status=SnapshotStatusRecord(state=SnapshotState.READY),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.get_snapshot_repository",
        lambda: repo,
    )

    with pytest.raises(HTTPException) as exc_info:
        await resolve_sandbox_image_from_request(
            CreateSandboxRequest(snapshotId="snap-image")
        )
    assert exc_info.value.status_code == 400
    assert "resourceLimits" in exc_info.value.detail["message"]


@pytest.mark.asyncio
async def test_snapshot_restore_rejects_unready_snapshot(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-002",
            source_sandbox_id="sbx-001",
            restore_config=SnapshotRestoreConfig(image="registry.example.com/snapshots/snap-002:latest"),
            status=SnapshotStatusRecord(
                state=SnapshotState.CREATING,
                last_transition_at=datetime.now(timezone.utc),
            ),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.get_snapshot_repository",
        lambda: repo,
    )

    request = CreateSandboxRequest(
        snapshotId="snap-002",
        resourceLimits=ResourceLimits(root={"cpu": "500m"}),
    )

    with pytest.raises(HTTPException) as exc_info:
        await resolve_sandbox_image_from_request(request)
    assert exc_info.value.status_code == 409


@pytest.mark.asyncio
async def test_snapshot_restore_passthrough_without_snapshot_id(monkeypatch, tmp_path) -> None:
    """Pool-only and image-backed creates pass through: no repo access, no 400."""
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.get_snapshot_repository",
        lambda: repo,
    )

    pool_only = CreateSandboxRequest(
        resourceLimits=ResourceLimits(root={"cpu": "1"}),
        extensions={"poolRef": "pool-a"},
        timeout=3600,
        entrypoint=["tail", "-f", "/dev/null"],
    )
    resolved = await resolve_sandbox_image_from_request(pool_only)
    assert resolved is pool_only
    assert resolved.resolved_snapshot_backend is None


@pytest.mark.asyncio
async def test_kata_snapshot_restore_attaches_private_plan_without_image_or_resources(
    monkeypatch,
    tmp_path,
) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    restore_config = SnapshotRestoreConfig(
        backend="kata-vmstate-v1",
        format="kata-vmstate-v1",
        restore_plan_secret_name="kata-restore-plan",
        restore_plan_owner_name="osb-snap-kata",
        restore_plan_owner_uid="snapshot-uid",
        source_node_name="node-a",
        snapshot_name="kata-snapshot-a",
        runtime_version="3.8.0",
        runtime_class_name="kata-vm-isolation-v2",
    )
    repo.create(
        SnapshotRecord(
            id="snap-kata",
            source_sandbox_id="sbx-001",
            restore_config=restore_config,
            status=SnapshotStatusRecord(state=SnapshotState.READY),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.get_snapshot_repository",
        lambda: repo,
    )

    request = CreateSandboxRequest(snapshotId="snap-kata", timeout=600, metadata={"team": "a"})
    resolved = await resolve_sandbox_image_from_request(request)

    assert resolved.image is None
    assert resolved.entrypoint is None
    assert resolved.resource_limits is None
    assert resolved.snapshot_restore_config == restore_config
    assert "pod_template" not in resolved.snapshot_restore_config.to_dict()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("entrypoint", ["sh"]),
        ("env", {"A": "B"}),
        ("resourceLimits", {"cpu": "1"}),
        ("resourceRequests", {"cpu": "1"}),
        ("platform", {"os": "linux", "arch": "amd64"}),
        ("networkPolicy", {"defaultAction": "allow", "egress": []}),
        ("credentialProxy", {"enabled": False}),
        ("secureAccess", False),
        ("volumes", []),
        ("lifecycle", {"preStart": {"command": ["true"]}}),
        ("extensions", {}),
    ],
)
async def test_kata_snapshot_restore_rejects_workload_shape_fields(
    monkeypatch,
    tmp_path,
    field,
    value,
) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        SnapshotRecord(
            id="snap-kata",
            source_sandbox_id="sbx-001",
            restore_config=SnapshotRestoreConfig(
                format="kata-vmstate-v1",
                restore_plan_secret_name="kata-restore-plan",
                restore_plan_owner_name="osb-snap-kata",
                restore_plan_owner_uid="snapshot-uid",
                source_node_name="node-a",
                snapshot_name="kata-snapshot-a",
                runtime_version="3.8.0",
                runtime_class_name="kata-vm-isolation-v2",
            ),
            status=SnapshotStatusRecord(state=SnapshotState.READY),
        )
    )
    monkeypatch.setattr(
        "opensandbox_server.services.snapshot_restore.get_snapshot_repository",
        lambda: repo,
    )
    request = CreateSandboxRequest.model_validate({"snapshotId": "snap-kata", field: value})

    with pytest.raises(HTTPException) as exc_info:
        await resolve_sandbox_image_from_request(request)

    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == "SANDBOX::INVALID_PARAMETER"
    assert exc_info.value.detail["message"] == (
        "kata-vmstate-v1 snapshot restore accepts only snapshotId, timeout, and metadata; "
        f"conflicting fields: {field}."
    )

    image_backed = CreateSandboxRequest(
        image=ImageSpec(uri="registry.example.com/app:1"),
        resourceLimits=ResourceLimits(root={"cpu": "1"}),
        timeout=3600,
        entrypoint=["tail", "-f", "/dev/null"],
    )
    resolved = await resolve_sandbox_image_from_request(image_backed)
    assert resolved is image_backed
    assert resolved.resolved_snapshot_backend is None
