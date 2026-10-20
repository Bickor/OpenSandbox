package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPrepareQEMUPublicRestore(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	snapshot := &sandboxv1alpha1.SandboxSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "default", UID: "uid"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{Format: sandboxv1alpha1.SandboxSnapshotFormatQEMUV1, SourceNodeName: "node",
			Containers: []sandboxv1alpha1.ContainerSnapshot{{ContainerName: "sandbox", ImageURI: "registry/rootfs:tag", ImageDigest: digest}},
			VirtualMachine: &sandboxv1alpha1.VirtualMachineSnapshot{ImageURI: "registry/vm:tag", ImageDigest: digest, PayloadDigest: digest,
				ManifestDigest: digest, SizeBytes: 1024, Compression: "zstd", Compatibility: sandboxv1alpha1.QEMUCompatibility{
					Architecture: "amd64", QEMUVersion: "6.2.0", MachineType: "pc-q35-6.2", CPUModel: "host", VCPUs: 2, MemoryBytes: 2147483648, QEMUConfigDigest: digest}}}}
	r := newTestSnapshotReconciler(snapshot)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"sandbox.opensandbox.io/checkpoint-provider": "qemu", "sandbox.opensandbox.io/qemu-container": "sandbox"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox", Image: "original"}}}}
	template, err := kataPodTemplate(pod)
	require.NoError(t, err)
	_, err = r.ensureKataRestorePlanSecret(context.Background(), snapshot, "snap-source", template.Raw)
	require.NoError(t, err)
	name, err := r.prepareQEMUPublicRestore(context.Background(), snapshot)
	require.NoError(t, err)
	secret := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, secret))
	var restored corev1.PodTemplateSpec
	require.NoError(t, json.Unmarshal(secret.Data[kataRestorePlanSecretKey], &restored))
	require.Equal(t, "registry/rootfs@"+digest, restored.Spec.Containers[0].Image)
	require.Equal(t, "registry/vm@"+digest, restored.Spec.InitContainers[0].Image)
	require.Equal(t, "node", restored.Spec.NodeName)
	require.True(t, *secret.Immutable)
	_, err = r.prepareQEMUPublicRestore(context.Background(), snapshot)
	require.NoError(t, err, "reconciliation must be idempotent")
}
