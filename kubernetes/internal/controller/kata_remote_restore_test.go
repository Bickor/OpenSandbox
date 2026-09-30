package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func remoteControllerFixture() (*sandboxv1alpha1.SandboxSnapshot, *sandboxv1alpha1.BatchSandbox) {
	snapshot := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "default", UID: "snapshot-uid"},
		Status: sandboxv1alpha1.SandboxSnapshotStatus{Phase: sandboxv1alpha1.SandboxSnapshotPhaseSucceed, Format: sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1, SourceNodeName: "node-a", SourcePodName: "source-pod",
			KataVMState: &sandboxv1alpha1.KataVMStateSnapshot{SnapshotName: "ks-0123456789abcdef0123456789abcdef", RestorePlanSecretName: "private-plan", BlobAccountURL: "https://account.blob.core.windows.net", BlobContainer: "snapshots", ManifestDigest: strings.Repeat("a", 64)},
		},
	}
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Name: "restored", Namespace: "default", UID: "restored-uid", Annotations: map[string]string{kataRestoreSnapshotAnnotation: snapshot.Name, kataRestoreOwnerAnnotation: string(snapshot.UID)}},
		Spec: sandboxv1alpha1.BatchSandboxSpec{Replicas: ptrToInt32(1), Template: &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{kataSnapshotAnnotation: snapshot.Status.KataVMState.SnapshotName}}, Spec: corev1.PodSpec{NodeName: "node-a"}}},
	}
	return snapshot, bs
}

func TestKataRemotePrepareGatesPodCreation(t *testing.T) {
	snapshot, bs := remoteControllerFixture()
	jobs := newTestSnapshotReconciler(snapshot, bs)
	jobs.ImageCommitterPodTemplate = &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"azure.workload.identity/use": "true"}}, Spec: corev1.PodSpec{ServiceAccountName: "snapshot-committer"}}
	r := &BatchSandboxReconciler{Client: jobs.Client, Scheme: jobs.Scheme, KataSnapshotJobs: jobs}
	ctx := context.Background()
	ready, err := r.prepareKataRestore(ctx, bs)
	require.NoError(t, err)
	require.False(t, ready)
	job := &batchv1.Job{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: "kata-prepare-restored-uid", Namespace: "default"}, job))
	require.True(t, metav1.IsControlledBy(job, bs))
	require.Nil(t, job.Spec.TTLSecondsAfterFinished)
	require.Equal(t, "node-a", job.Spec.Template.Spec.NodeName)
	require.Equal(t, "snapshot-committer", job.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, "true", job.Spec.Template.Labels["azure.workload.identity/use"])
	require.Equal(t, []string{"kata-vmstate", "prepare", "--snapshot-name", snapshot.Status.KataVMState.SnapshotName}, job.Spec.Template.Spec.Containers[0].Args)
	ready, err = r.prepareKataRestore(ctx, bs)
	require.NoError(t, err)
	require.False(t, ready)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(t, r.Status().Update(ctx, job))
	ready, err = r.prepareKataRestore(ctx, bs)
	require.NoError(t, err)
	require.True(t, ready)
}

func TestKataRemotePrepareRejectsOwnerAndTemplateDrift(t *testing.T) {
	for _, mutation := range []string{"owner", "node", "snapshot", "pool"} {
		t.Run(mutation, func(t *testing.T) {
			snapshot, bs := remoteControllerFixture()
			jobs := newTestSnapshotReconciler(snapshot, bs)
			r := &BatchSandboxReconciler{Client: jobs.Client, Scheme: jobs.Scheme, KataSnapshotJobs: jobs}
			switch mutation {
			case "owner":
				bs.Annotations[kataRestoreOwnerAnnotation] = "wrong"
			case "node":
				bs.Spec.Template.Spec.NodeName = "wrong"
			case "snapshot":
				bs.Spec.Template.Annotations[kataSnapshotAnnotation] = "wrong"
			case "pool":
				bs.Spec.PoolRef = "pool"
			}
			ready, err := r.prepareKataRestore(context.Background(), bs)
			require.Error(t, err)
			require.False(t, ready)
		})
	}
}

func TestKataRemoteCleanupWithoutSourceNode(t *testing.T) {
	snapshot, _ := remoteControllerFixture()
	r := newTestSnapshotReconciler(snapshot)
	complete, _, err := r.ensureKataCleanup(context.Background(), snapshot)
	require.NoError(t, err)
	require.False(t, complete)
	job := &batchv1.Job{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snapshot-kata-cleanup-blob"}, job))
	require.Empty(t, job.Spec.Template.Spec.NodeName)
	require.Empty(t, job.Spec.Template.Spec.Volumes)
	require.Equal(t, "delete-remote", job.Spec.Template.Spec.Containers[0].Args[1])
}

func TestKataRemoteCaptureMountsPrivatePlan(t *testing.T) {
	snapshot, _ := remoteControllerFixture()
	r := newTestSnapshotReconciler(snapshot)
	job, err := r.buildKataVMStateJob(snapshot, "source-uid", false)
	require.NoError(t, err)
	require.Contains(t, job.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "KATA_SNAPSHOT_BLOB_ACCOUNT_URL", Value: snapshot.Status.KataVMState.BlobAccountURL})
	require.Contains(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "restore-plan", MountPath: "/restore-plan", ReadOnly: true})
}
