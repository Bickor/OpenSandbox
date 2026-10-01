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

from copy import deepcopy
from datetime import datetime, timezone
from typing import Any, cast
from unittest.mock import AsyncMock, MagicMock

import pytest
from fastapi import HTTPException
from kubernetes.client import ApiException

from opensandbox_server.api import pool as pool_api
from opensandbox_server.api.schema import CreatePoolRequest, CreateSandboxRequest, ImageSpec
from opensandbox_server.config import AppConfig, KubernetesRuntimeConfig, RuntimeConfig
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.extensions.keys import ACCESS_RENEW_EXTEND_SECONDS_KEY
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
from opensandbox_server.services.k8s.pool_allocation import (
    ALLOCATION_MODE_ANNOTATION,
    RESERVED_ALLOCATION_ANNOTATIONS,
    UID_BOUND_ALLOCATION_MODE,
)
from opensandbox_server.services.k8s.pool_service import PoolService


def _config(enabled=False):
    return AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
        kubernetes=KubernetesRuntimeConfig(
            namespace="protected-ns",
            protected_pool_allocations=enabled,
        ),
    )


def _pool_body():
    return {
        "name": "warm-pool",
        "template": {
            "metadata": {"annotations": {"example.com/template": "preserved"}},
            "spec": {"containers": [{"name": "sandbox", "image": "python:3.11"}]},
        },
        "capacitySpec": {"bufferMin": 1, "bufferMax": 2, "poolMin": 1, "poolMax": 3},
    }


def _live_pool():
    return {
        "apiVersion": "sandbox.opensandbox.io/v1alpha1",
        "kind": "Pool",
        "metadata": {
            "name": "warm-pool",
            "namespace": "protected-ns",
            "uid": "pool-uid",
            "annotations": {ALLOCATION_MODE_ANNOTATION: UID_BOUND_ALLOCATION_MODE},
        },
        "spec": {"recycleStrategy": {"type": "Delete"}},
    }


def _create_batch(provider, **overrides):
    kwargs = {
        "sandbox_id": "sandbox-id",
        "namespace": "protected-ns",
        "image_spec": ImageSpec(uri="unused:pool-mode", auth=None),
        "entrypoint": ["sleep", "60"],
        "env": {"EXAMPLE": "value"},
        "resource_limits": {},
        "labels": {"example.com/label": "preserved"},
        "expires_at": None,
        "execd_image": "unused:pool-mode",
        "extensions": {"poolRef": "warm-pool"},
    }
    if provider.protected_pool_allocations:
        kwargs.update(image_spec=None, entrypoint=None, env={})
    kwargs.update(overrides)
    return provider.create_workload(**kwargs)


@pytest.mark.parametrize("enabled", [None, False, True])
def test_pool_route_uses_operator_setting_on_initial_create(
    enabled, monkeypatch, mock_k8s_client, client, auth_headers
):
    config = _config(enabled) if enabled is not None else AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
    )
    assert config.kubernetes is not None
    config.kubernetes.namespace = "protected-ns"
    monkeypatch.setattr(pool_api, "get_config", lambda: config)
    monkeypatch.setattr(
        "opensandbox_server.services.k8s.client.K8sClient", lambda _: mock_k8s_client
    )
    custom_api = mock_k8s_client.get_custom_objects_api()
    custom_api.create_namespaced_custom_object.side_effect = lambda **kwargs: kwargs["body"]
    request = _pool_body()
    # Unknown request fields keep their native acceptance, but never become config.
    request.update({
        "protected_pool_allocations": not enabled,
        "allocationMode": "attacker-mode",
        "namespace": "foreign-ns",
        "metadata": {"annotations": {ALLOCATION_MODE_ANNOTATION: "attacker-mode"}},
        "recycleStrategy": {"type": "Recreate"},
        "networkPolicy": {"defaultAction": "deny", "egress": []},
    })
    response = client.post("/pools", json=request, headers=auth_headers)
    assert response.status_code == 201
    assert response.json() == {
        "name": "warm-pool",
        "capacitySpec": request["capacitySpec"],
    }
    expected = {
        "apiVersion": "sandbox.opensandbox.io/v1alpha1",
        "kind": "Pool",
        "metadata": {"name": "warm-pool", "namespace": "protected-ns"},
        "spec": {
            "template": request["template"],
            "capacitySpec": request["capacitySpec"],
        },
    }
    if enabled:
        expected["metadata"]["annotations"] = {
            ALLOCATION_MODE_ANNOTATION: UID_BOUND_ALLOCATION_MODE,
        }
        expected["spec"]["recycleStrategy"] = {"type": "Delete"}
    custom_api.create_namespaced_custom_object.assert_called_once_with(
        group="sandbox.opensandbox.io",
        version="v1alpha1",
        namespace="protected-ns",
        plural="pools",
        body=expected,
    )
    custom_api.patch_namespaced_custom_object.assert_not_called()


@pytest.mark.parametrize("annotation", sorted(RESERVED_ALLOCATION_ANNOTATIONS))
@pytest.mark.parametrize("enabled", [False, True])
def test_pool_template_reserved_annotations_are_guarded_only_when_protected(
    annotation, enabled, mock_k8s_client
):
    service = PoolService(
        mock_k8s_client, "protected-ns", protected_pool_allocations=enabled
    )
    body = _pool_body()
    body["template"]["metadata"]["annotations"][annotation] = UID_BOUND_ALLOCATION_MODE
    request = CreatePoolRequest.model_validate(body)
    before = deepcopy(request.template)
    custom_api = mock_k8s_client.get_custom_objects_api()
    custom_api.create_namespaced_custom_object.side_effect = lambda **kwargs: kwargs["body"]
    if enabled:
        with pytest.raises(HTTPException) as error:
            service.create_pool(request)
        assert error.value.status_code == 400
        detail = cast(dict[str, Any], error.value.detail)
        assert isinstance(detail, dict)
        assert detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
        assert annotation in detail["message"]
        custom_api.create_namespaced_custom_object.assert_not_called()
    else:
        service.create_pool(request)
        assert custom_api.create_namespaced_custom_object.call_args.kwargs["body"] == {
            "apiVersion": "sandbox.opensandbox.io/v1alpha1",
            "kind": "Pool",
            "metadata": {"name": "warm-pool", "namespace": "protected-ns"},
            "spec": {"template": before, "capacitySpec": body["capacitySpec"]},
        }
    assert request.template == before


@pytest.mark.parametrize("config", [
    None,
    AppConfig(runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test")),
    _config(False),
])
@pytest.mark.parametrize("annotations", [
    None,
    {},
    {"example.com/annotation": "preserved"},
    {"sandbox.opensandbox.io/alloc-status": '{"pods":["native-pod"]}'},
])
def test_native_batch_no_config_or_false_retains_manifest_and_does_not_read_pool(
    config, annotations, mock_k8s_client
):
    provider = BatchSandboxProvider(mock_k8s_client, config)
    result = _create_batch(provider, annotations=annotations)
    body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
    metadata = {
        "name": "sandbox-id",
        "namespace": "protected-ns",
        "labels": {"example.com/label": "preserved"},
    }
    if annotations:
        metadata["annotations"] = annotations
    assert body == {
        "apiVersion": "sandbox.opensandbox.io/v1alpha1",
        "kind": "BatchSandbox",
        "metadata": metadata,
        "spec": {
            "replicas": 1,
            "poolRef": "warm-pool",
            "taskTemplate": provider._build_task_template(
                ["sleep", "60"], {"EXAMPLE": "value"}, "sandbox-id"
            ),
        },
    }
    assert result == {
        "name": "test", "uid": "uid",
        "apiVersion": "sandbox.opensandbox.io/v1alpha1", "kind": "BatchSandbox",
    }
    mock_k8s_client.get_custom_object.assert_not_called()
    mock_k8s_client.get_custom_objects_api.assert_not_called()
    mock_k8s_client.patch_custom_object.assert_not_called()


def test_protected_setting_leaves_nonpooled_creation_unchanged(mock_k8s_client):
    native = BatchSandboxProvider(mock_k8s_client, _config(False))
    _create_batch(native, extensions={})
    native_kwargs = deepcopy(mock_k8s_client.create_custom_object.call_args.kwargs)
    protected = BatchSandboxProvider(mock_k8s_client, _config(True))
    _create_batch(
        protected, extensions={}, image_spec=ImageSpec(uri="unused:pool-mode", auth=None),
        entrypoint=["sleep", "60"], env={"EXAMPLE": "value"},
    )
    assert mock_k8s_client.create_custom_object.call_args.kwargs == native_kwargs
    mock_k8s_client.get_custom_objects_api.assert_not_called()


@pytest.mark.parametrize("strategy", [None, {}, {"type": ""}, {"type": "Delete"}])
def test_protected_batch_reads_live_pool_and_marks_initial_single_replica_create(
    strategy, mock_k8s_client, monkeypatch
):
    provider = BatchSandboxProvider(mock_k8s_client, _config(True))
    build_task = MagicMock(side_effect=AssertionError("Passive allocations cannot bootstrap"))
    monkeypatch.setattr(provider, "_build_task_template", build_task)
    pool = _live_pool()
    if strategy is None:
        del pool["spec"]["recycleStrategy"]
    else:
        pool["spec"]["recycleStrategy"] = strategy
    api = mock_k8s_client.get_custom_objects_api()
    api.get_namespaced_custom_object.return_value = pool
    annotations = {"example.com/annotation": "preserved"}
    events = []
    api.get_namespaced_custom_object.side_effect = lambda **kwargs: events.append("get") or pool

    def create(**kwargs):
        events.append("create")
        assert kwargs["body"]["metadata"]["annotations"] == {
            **annotations, ALLOCATION_MODE_ANNOTATION: UID_BOUND_ALLOCATION_MODE,
        }
        return {"metadata": {"name": "sandbox-id", "uid": "batch-uid"}}

    mock_k8s_client.create_custom_object.side_effect = create
    result = _create_batch(provider, annotations=annotations)
    assert events == ["get", "create"]
    assert result == {
        "name": "sandbox-id", "uid": "batch-uid",
        "apiVersion": "sandbox.opensandbox.io/v1alpha1", "kind": "BatchSandbox",
    }
    body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
    assert body["metadata"]["namespace"] == "protected-ns"
    assert body["spec"]["replicas"] == 1
    assert body["spec"]["poolRef"] == "warm-pool"
    assert set(body["spec"]) == {"replicas", "poolRef"}
    build_task.assert_not_called()
    assert annotations == {"example.com/annotation": "preserved"}
    api.get_namespaced_custom_object.assert_called_once_with(
        group="sandbox.opensandbox.io", version="v1alpha1",
        namespace="protected-ns", plural="pools", name="warm-pool",
    )
    mock_k8s_client.get_custom_object.assert_not_called()
    mock_k8s_client.patch_custom_object.assert_not_called()


@pytest.mark.parametrize(
    "field,value",
    [
        ("annotations", {}),
        ("annotations", {ALLOCATION_MODE_ANNOTATION: ""}),
        ("annotations", {ALLOCATION_MODE_ANNOTATION: "native"}),
        ("annotations", {ALLOCATION_MODE_ANNOTATION: "uid-bound-v2"}),
        ("annotations", {"example.com/allocation-mode": UID_BOUND_ALLOCATION_MODE}),
        ("annotations", [ALLOCATION_MODE_ANNOTATION]),
        ("namespace", "foreign-ns"),
        ("namespace", None),
        ("name", "foreign-pool"),
        ("uid", ""),
        ("uid", None),
        ("deletionTimestamp", "2026-09-30T00:00:00Z"),
    ],
)
def test_protected_batch_rejects_untrusted_pool_before_create(
    field, value, mock_k8s_client
):
    pool = _live_pool()
    pool["metadata"][field] = value
    mock_k8s_client.get_custom_object.return_value = _live_pool()
    mock_k8s_client.get_custom_objects_api().get_namespaced_custom_object.return_value = pool
    provider = BatchSandboxProvider(mock_k8s_client, _config(True))
    with pytest.raises(ValueError, match="created with allocation-mode=uid-bound-v1"):
        _create_batch(provider)
    mock_k8s_client.create_custom_object.assert_not_called()
    mock_k8s_client.get_custom_object.assert_not_called()


@pytest.mark.parametrize("strategy", [{"type": "Recreate"}, {"type": "Reuse"}, "Delete"])
def test_protected_batch_rejects_non_delete_pool(strategy, mock_k8s_client):
    pool = _live_pool()
    pool["spec"]["recycleStrategy"] = strategy
    mock_k8s_client.get_custom_objects_api().get_namespaced_custom_object.return_value = pool
    provider = BatchSandboxProvider(mock_k8s_client, _config(True))
    with pytest.raises(ValueError, match="requires Delete recycling"):
        _create_batch(provider)
    mock_k8s_client.create_custom_object.assert_not_called()


def test_protected_batch_rejects_missing_pool(mock_k8s_client):
    api = mock_k8s_client.get_custom_objects_api()
    api.get_namespaced_custom_object.side_effect = ApiException(status=404)
    with pytest.raises(ValueError, match="does not exist"):
        _create_batch(BatchSandboxProvider(mock_k8s_client, _config(True)))
    mock_k8s_client.create_custom_object.assert_not_called()


@pytest.mark.parametrize("pool", [
    None,
    {},
    {"metadata": None},
    {"metadata": []},
    {**_live_pool(), "spec": None},
])
def test_protected_batch_rejects_malformed_pool_response(pool, mock_k8s_client):
    api = mock_k8s_client.get_custom_objects_api()
    api.get_namespaced_custom_object.return_value = pool
    with pytest.raises(ValueError, match="invalid"):
        _create_batch(BatchSandboxProvider(mock_k8s_client, _config(True)))
    mock_k8s_client.create_custom_object.assert_not_called()


def test_protected_batch_fails_closed_when_pool_read_is_forbidden(mock_k8s_client):
    api = mock_k8s_client.get_custom_objects_api()
    api.get_namespaced_custom_object.side_effect = ApiException(status=403)
    with pytest.raises(ApiException):
        _create_batch(BatchSandboxProvider(mock_k8s_client, _config(True)))
    mock_k8s_client.create_custom_object.assert_not_called()


def test_protected_batch_rejects_auto_assignment_without_creating(mock_k8s_client):
    with pytest.raises(ValueError, match="explicit poolRef"):
        _create_batch(
            BatchSandboxProvider(mock_k8s_client, _config(True)),
            extensions={"poolRef": "*"},
        )
    mock_k8s_client.create_custom_object.assert_not_called()
    mock_k8s_client.get_custom_objects_api.assert_not_called()


@pytest.mark.parametrize("annotation", sorted(RESERVED_ALLOCATION_ANNOTATIONS))
def test_protected_batch_cannot_receive_caller_allocation_annotations(
    annotation, mock_k8s_client
):
    with pytest.raises(ValueError, match="operator/controller-managed"):
        _create_batch(
            BatchSandboxProvider(mock_k8s_client, _config(True)),
            annotations={annotation: UID_BOUND_ALLOCATION_MODE},
        )
    mock_k8s_client.create_custom_object.assert_not_called()
    mock_k8s_client.get_custom_objects_api.assert_not_called()


@pytest.mark.parametrize("enabled", [False, True])
@pytest.mark.asyncio
async def test_request_metadata_extensions_and_extra_fields_cannot_choose_allocation_mode(
    enabled, k8s_service, mock_k8s_client, monkeypatch
):
    config = _config(enabled)
    provider = BatchSandboxProvider(mock_k8s_client, config)
    k8s_service.app_config = config
    k8s_service.namespace = "protected-ns"
    k8s_service.k8s_client = mock_k8s_client
    k8s_service.workload_provider = provider
    mock_k8s_client.get_custom_object.return_value = _live_pool()
    mock_k8s_client.get_custom_objects_api().get_namespaced_custom_object.return_value = _live_pool()
    monkeypatch.setattr(k8s_service, "_wait_for_sandbox_ready", AsyncMock(return_value={
        "metadata": {}, "spec": {}, "status": {"phase": "Running"},
    }))
    request = CreateSandboxRequest.model_validate({
        "extensions": {
            "poolRef": "warm-pool",
            "protected_pool_allocations": str(not enabled),
            "allocationMode": "attacker-mode",
            **{key: "attacker-value" for key in RESERVED_ALLOCATION_ANNOTATIONS},
            "opensandbox.extensions.sandbox.opensandbox.io/allocation-mode": "attacker-mode",
        },
        "metadata": {key: "attacker-value" for key in RESERVED_ALLOCATION_ANNOTATIONS},
        "annotations": {key: "attacker-value" for key in RESERVED_ALLOCATION_ANNOTATIONS},
        "protected_pool_allocations": not enabled,
        "replicas": 5,
    })
    if enabled:
        with pytest.raises(HTTPException) as error:
            await k8s_service.create_sandbox(request)
        assert error.value.status_code == 400
        assert "fixed prestarted workload" in str(error.value.detail)
        mock_k8s_client.get_custom_object.assert_not_called()
        mock_k8s_client.create_custom_object.assert_not_called()
        return
    await k8s_service.create_sandbox(request)
    body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
    annotations = body["metadata"].get("annotations", {})
    allocation_annotations = {
        key: annotations[key] for key in RESERVED_ALLOCATION_ANNOTATIONS if key in annotations
    }
    assert allocation_annotations == (
        {ALLOCATION_MODE_ANNOTATION: UID_BOUND_ALLOCATION_MODE} if enabled else {}
    )
    assert body["spec"]["replicas"] == 1
    assert body["metadata"]["labels"][ALLOCATION_MODE_ANNOTATION] == "attacker-value"
    assert annotations[
        "opensandbox.io/extensions.sandbox.opensandbox.io/allocation-mode"
    ] == "attacker-mode"


@pytest.mark.parametrize("enabled", [False, True])
@pytest.mark.asyncio
async def test_network_policy_is_still_rejected_in_pool_mode(enabled, k8s_service):
    k8s_service.app_config = _config(enabled)
    request = CreateSandboxRequest.model_validate({
        "extensions": {"poolRef": "warm-pool"},
        "networkPolicy": {"defaultAction": "deny", "egress": []},
    })
    with pytest.raises(HTTPException) as error:
        await k8s_service.create_sandbox(request)
    assert error.value.status_code == 400
    detail = cast(dict[str, Any], error.value.detail)
    assert isinstance(detail, dict)
    assert detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
    assert "networkPolicy cannot be used together with extensions.poolRef" in (
        detail["message"]
    )
    k8s_service.k8s_client.get_custom_object.assert_not_called()
    k8s_service.workload_provider.create_workload.assert_not_called()


@pytest.mark.parametrize("field,value", [
    ("image", {"uri": "busybox:latest"}),
    ("image", {"uri": "private/image", "auth": {"username": "user", "password": "password"}}),
    ("entrypoint", ["sleep", "60"]),
    ("env", {}),
    ("env", {"CUSTOM": "value"}),
    ("env", {"OPENSANDBOX_EGRESS_EXTRA": "must-not-be-stripped"}),
    ("resourceLimits", {"cpu": "1", "memory": "1Gi"}),
    ("resourceRequests", {"cpu": "100m"}),
    ("volumes", []),
    ("platform", {"os": "linux", "arch": "amd64"}),
    ("credentialProxy", {"enabled": False}),
    ("credentialProxy", {"enabled": True}),
    ("secureAccess", True),
    ("networkPolicy", {"defaultAction": "deny", "egress": []}),
    ("templateId", "some-template"),
    ("taskTemplate", {"spec": {"process": {"command": ["sh"]}}}),
    ("replicas", 2),
    ("annotations", {"opensandbox.io/secure-access-token": "forged"}),
    ("auth", {"token": "forged"}),
])
@pytest.mark.asyncio
async def test_passive_request_rejects_overrides_before_any_allocation(
    field, value, k8s_service, monkeypatch
):
    k8s_service.app_config = _config(True)
    build_context = MagicMock(side_effect=AssertionError("Overrides must fail before context"))
    monkeypatch.setattr(
        "opensandbox_server.services.k8s.kubernetes_service._build_create_workload_context",
        build_context,
    )
    request = CreateSandboxRequest.model_validate({
        "extensions": {"poolRef": "warm-pool"}, "timeout": 600, field: value,
    })
    with pytest.raises(HTTPException) as error:
        await k8s_service.create_sandbox(request)
    assert error.value.status_code == 400
    assert field in str(error.value.detail)
    build_context.assert_not_called()
    k8s_service.k8s_client.get_custom_object.assert_not_called()
    k8s_service.workload_provider.create_workload.assert_not_called()


@pytest.mark.parametrize("extension", [
    "bootstrap.execd.isolation",
    "bootstrap.execd.preinstalled",
    "opensandbox.extensions.command",
    "allocationMode",
    ALLOCATION_MODE_ANNOTATION,
])
@pytest.mark.asyncio
async def test_passive_request_rejects_bootstrap_and_unknown_extensions(extension, k8s_service):
    k8s_service.app_config = _config(True)
    request = CreateSandboxRequest.model_validate({
        "extensions": {"poolRef": "warm-pool", extension: "enable"},
    })
    with pytest.raises(HTTPException) as error:
        await k8s_service.create_sandbox(request)
    assert error.value.status_code == 400
    assert extension in str(error.value.detail)
    k8s_service.k8s_client.get_custom_object.assert_not_called()
    k8s_service.workload_provider.create_workload.assert_not_called()


@pytest.mark.parametrize("overrides", [
    {"image_spec": ImageSpec(uri="override:image", auth=None)},
    {"entrypoint": ["sleep", "60"]},
    {"env": {"EXAMPLE": "override"}},
    {"resource_limits": {"cpu": "1"}},
    {"resource_requests": {"cpu": "100m"}},
    {"volumes": []},
    {"extensions": {"poolRef": "warm-pool", "bootstrap.execd.preinstalled": "enable"}},
    {"annotations": {"opensandbox.io/secure-access-token": "forged"}},
    {"annotations": {"opensandbox.io/egress-auth-token": "forged"}},
])
def test_passive_provider_rejects_overrides_before_pool_read(overrides, mock_k8s_client):
    with pytest.raises(ValueError, match="do not accept workload/auth overrides"):
        _create_batch(BatchSandboxProvider(mock_k8s_client, _config(True)), **overrides)
    mock_k8s_client.get_custom_objects_api.assert_not_called()
    mock_k8s_client.create_custom_object.assert_not_called()


@pytest.mark.asyncio
async def test_passive_create_succeeds_from_pod_readiness_without_runtime_task(
    k8s_service, mock_k8s_client, monkeypatch
):
    config = _config(True)
    provider = BatchSandboxProvider(mock_k8s_client, config)
    k8s_service.app_config = config
    k8s_service.namespace = "protected-ns"
    k8s_service.k8s_client = mock_k8s_client
    k8s_service.workload_provider = provider
    pool = _live_pool()
    mock_k8s_client.get_custom_objects_api().get_namespaced_custom_object.return_value = pool
    created = {}

    def create(**kwargs):
        created.update(deepcopy(kwargs["body"]))
        created["metadata"]["uid"] = "batch-uid"
        created["metadata"]["creationTimestamp"] = datetime.now(timezone.utc).isoformat()
        created["status"] = {"phase": "Succeed", "ready": 1, "allocated": 1, "replicas": 1}
        return created

    mock_k8s_client.create_custom_object.side_effect = create
    mock_k8s_client.get_custom_object.side_effect = (
        lambda **kwargs: pool if kwargs["plural"] == "pools" else created
    )
    build_task = MagicMock(side_effect=AssertionError("No bootstrap task allowed"))
    monkeypatch.setattr(provider, "_build_task_template", build_task)
    request = CreateSandboxRequest.model_validate({
        "extensions": {"poolRef": "warm-pool", ACCESS_RENEW_EXTEND_SECONDS_KEY: "600"},
        "timeout": 600,
        "metadata": {"example.com/owner": "test"},
        "secureAccess": False,
        "image": None,
        "entrypoint": None,
        "env": None,
    })
    response = await k8s_service.create_sandbox(request)
    assert response.status.state == "Running"
    assert response.entrypoint == []
    assert response.expires_at is not None
    assert set(created["spec"]) == {"replicas", "poolRef", "expireTime"}
    assert created["metadata"]["annotations"] == {
        ALLOCATION_MODE_ANNOTATION: UID_BOUND_ALLOCATION_MODE,
        "opensandbox.io/access-renew-extend-seconds": "600",
    }
    assert created["metadata"]["labels"]["example.com/owner"] == "test"
    build_task.assert_not_called()
    mock_k8s_client.patch_custom_object.assert_not_called()


def test_extra_field_tracking_does_not_change_native_schema_or_serialization():
    request = CreateSandboxRequest.model_validate({
        "extensions": {"poolRef": "warm-pool"},
        "taskTemplate": {"spec": {}},
        "auth": {"token": "ignored"},
    })
    ordinary = CreateSandboxRequest.model_validate({"extensions": {"poolRef": "warm-pool"}})
    assert request.model_dump() == ordinary.model_dump()
    assert request.extra_request_fields == frozenset({"taskTemplate", "auth"})
    assert "extra_request_fields" not in CreateSandboxRequest.model_json_schema()["properties"]


@pytest.mark.parametrize("payload", [
    {"env": {}},
    {"taskTemplate": {"spec": {"process": {"command": ["sh"]}}}},
    {"_extra_request_fields": [], "auth": {"token": "forged"}},
    {"secureAccess": False, "secure_access": True},
    {"networkPolicy": None, "network_policy": {"defaultAction": "deny", "egress": []}},
])
def test_passive_http_request_cannot_hide_overrides(
    payload, client, auth_headers, k8s_service, monkeypatch
):
    k8s_service.app_config = _config(True)
    monkeypatch.setattr("opensandbox_server.api.lifecycle.sandbox_service", k8s_service)
    response = client.post(
        "/sandboxes",
        json={"extensions": {"poolRef": "warm-pool"}, **payload},
        headers=auth_headers,
    )
    assert response.status_code == 400
    assert "fixed prestarted workload" in response.json()["message"]
    k8s_service.k8s_client.get_custom_object.assert_not_called()
    k8s_service.workload_provider.create_workload.assert_not_called()
