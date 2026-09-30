import base64
import json
from unittest.mock import MagicMock

import pytest

from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
from opensandbox_server.services.k8s.snapshot_runtime import KubernetesSnapshotRuntime
from opensandbox_server.services.snapshot_models import SnapshotRestoreConfig, SnapshotState


def test_qemu_catalog_and_restore_keep_prepared_images_and_runtime():
    snapshot = {
        "metadata": {"name": "snapshot", "uid": "snapshot-uid"},
        "status": {
            "format": "qemu-v1",
            "phase": "Succeed",
            "sourceNodeName": "node",
            "restorePlanSecretName": "plan",
            "virtualMachine": {"imageUri": "vm@sha256:abc"},
        },
    }
    client = MagicMock()
    runtime = KubernetesSnapshotRuntime(client, namespace="test")
    result = runtime._snapshot_status_from_cr(snapshot)
    assert result.state == SnapshotState.READY
    config = SnapshotRestoreConfig(
        format=result.format,
        source_node_name=result.source_node_name,
        restore_plan_secret_name=result.restore_plan_secret_name,
        restore_plan_owner_name=result.restore_plan_owner_name,
        restore_plan_owner_uid=result.restore_plan_owner_uid,
    )
    template = {
        "metadata": {"annotations": {"sandbox.opensandbox.io/checkpoint-provider": "qemu"}},
        "spec": {
            "nodeName": "node",
            "runtimeClassName": "qemu-runc",
            "containers": [{"name": "sandbox", "image": "rootfs@sha256:abc"}],
            "initContainers": [{"name": "opensandbox-vmstate-restore", "image": "vm@sha256:def"}],
        },
    }
    client.read_secret.return_value = {
        "type": "Opaque",
        "immutable": True,
        "metadata": {
            "ownerReferences": [
                {
                    "apiVersion": "sandbox.opensandbox.io/v1alpha1",
                    "kind": "SandboxSnapshot",
                    "name": "snapshot",
                    "uid": "snapshot-uid",
                    "controller": True,
                }
            ]
        },
        "data": {"pod-template.json": base64.b64encode(json.dumps(template).encode()).decode()},
    }
    client.read_node.return_value = {
        "status": {"conditions": [{"type": "Ready", "status": "True"}]}
    }
    client.create_custom_object.return_value = {"metadata": {"name": "restored", "uid": "new"}}
    provider = BatchSandboxProvider(client)
    provider.create_workload_from_qemu_snapshot(
        sandbox_id="restored",
        namespace="test",
        restore_config=config,
        labels={"opensandbox.io/sandbox-id": "restored"},
        annotations=None,
        expires_at=None,
    )
    body = client.create_custom_object.call_args.kwargs["body"]
    assert body["spec"]["replicas"] == 0
    assert body["spec"]["template"]["spec"] == template["spec"]
    client.read_secret.return_value["metadata"]["ownerReferences"][0]["uid"] = "wrong"
    with pytest.raises(ValueError, match="ownership"):
        provider.create_workload_from_qemu_snapshot(
            sandbox_id="restored",
            namespace="test",
            restore_config=config,
            labels={},
            annotations=None,
            expires_at=None,
        )


def test_qemu_catalog_never_accepts_an_image_without_restore_plan():
    runtime = KubernetesSnapshotRuntime(MagicMock(), namespace="test")
    result = runtime._snapshot_status_from_cr(
        {
            "status": {
                "format": "qemu-v1",
                "phase": "Succeed",
                "containers": [{"containerName": "sandbox", "imageUri": "rootfs"}],
            }
        }
    )
    assert result.state == SnapshotState.FAILED


def test_runtime_template_selection_is_request_local(tmp_path):
    from opensandbox_server.api.schema import ImageSpec
    from opensandbox_server.config import AppConfig, KubernetesRuntimeConfig, RuntimeConfig

    template = tmp_path / "qemu.yaml"
    template.write_text(
        "spec:\n  template:\n    metadata:\n      annotations:\n        sandbox.opensandbox.io/checkpoint-provider: qemu\n    spec:\n      containers:\n        - name: sandbox\n          securityContext:\n            privileged: true\n      volumes:\n        - name: kvm\n          hostPath:\n            path: /dev/kvm\n"
    )
    client = MagicMock()
    client.create_custom_object.side_effect = lambda **kw: {
        "metadata": {"name": kw["body"]["metadata"]["name"], "uid": "uid"}
    }
    provider = BatchSandboxProvider(
        client,
        AppConfig(
            runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
            kubernetes=KubernetesRuntimeConfig(
                runtime_class_templates={"qemu-runc": str(template)}
            ),
        ),
    )
    args = dict(
        sandbox_id="sandbox",
        namespace="test",
        image_spec=ImageSpec(uri="demo"),
        entrypoint=["/qemu"],
        env={},
        resource_limits={"cpu": "2", "memory": "4Gi"},
        labels={},
        expires_at=None,
        execd_image="execd:test",
    )
    provider.create_workload(
        **args,
        extensions={"runtimeClassName": "qemu-runc", "bootstrap.execd.preinstalled": "enable"},
    )
    body = client.create_custom_object.call_args.kwargs["body"]
    assert body["spec"]["template"]["spec"]["runtimeClassName"] == "qemu-runc"
    assert body["spec"]["template"]["spec"]["containers"][0]["securityContext"]["privileged"]
    assert (
        body["spec"]["template"]["metadata"]["annotations"][
            "sandbox.opensandbox.io/checkpoint-provider"
        ]
        == "qemu"
    )
    assert provider.runtime_class is None
    assert provider.template_manager.get_base_template() != body
    with pytest.raises(ValueError, match="operator-approved"):
        provider.create_workload(**args, extensions={"runtimeClassName": "arbitrary"})
