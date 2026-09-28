package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

const kataRestoreSnapshotAnnotation = "opensandbox.io/kata-restore-snapshot"
const kataRestoreOwnerAnnotation = "opensandbox.io/kata-restore-snapshot-uid"

func kataBlobEnv(state *sandboxv1alpha1.KataVMStateSnapshot) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "KATA_SNAPSHOT_BLOB_ACCOUNT_URL", Value: state.BlobAccountURL},
		{Name: "KATA_SNAPSHOT_BLOB_CONTAINER", Value: state.BlobContainer},
		{Name: "KATA_SNAPSHOT_MANIFEST_DIGEST", Value: state.ManifestDigest},
	}
}

// prepareKataRestore runs before scale-up. The server reserves the snapshot via
// a zero-replica BatchSandbox, revalidates deletion, and then activates it. The
// controller keeps Pods absent until the node preparation Job succeeds.
func (r *BatchSandboxReconciler) prepareKataRestore(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox) (bool, error) {
	name := bs.Annotations[kataRestoreSnapshotAnnotation]
	if name == "" || bs.Spec.Replicas == nil || *bs.Spec.Replicas == 0 || !bs.DeletionTimestamp.IsZero() {
		return true, nil
	}
	snapshot := &sandboxv1alpha1.SandboxSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: name}, snapshot); err != nil {
		return false, err
	}
	if string(snapshot.UID) != bs.Annotations[kataRestoreOwnerAnnotation] || !snapshot.DeletionTimestamp.IsZero() || snapshot.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseSucceed {
		return false, fmt.Errorf("Kata restore snapshot is no longer ready or has different ownership")
	}
	state := snapshot.Status.KataVMState
	if state == nil {
		return false, fmt.Errorf("missing Kata restore metadata")
	}
	if state.BlobAccountURL == "" {
		return true, nil
	}
	if r.KataSnapshotJobs == nil {
		return false, fmt.Errorf("remote Kata preparation is not configured")
	}
	if bs.Spec.PoolRef != "" || bs.Spec.Template == nil || bs.Spec.Template.Spec.NodeName != snapshot.Status.SourceNodeName || bs.Spec.Template.Annotations[kataSnapshotAnnotation] != state.SnapshotName {
		return false, fmt.Errorf("remote Kata restore template does not match snapshot")
	}
	jobName := fmt.Sprintf("kata-prepare-%s", bs.UID)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: bs.Namespace, Name: jobName}, job)
	if err == nil {
		if !metav1.IsControlledBy(job, bs) {
			return false, fmt.Errorf("Kata preparation Job ownership mismatch")
		}
		if failed := findJobCondition(job.Status.Conditions, batchv1.JobFailed); failed != nil {
			return false, fmt.Errorf("Kata remote preparation failed: %s", failed.Message)
		}
		if findJobCondition(job.Status.Conditions, batchv1.JobComplete) != nil {
			return true, nil
		}
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	job, err = r.KataSnapshotJobs.buildKataVMStateJob(snapshot, "", true)
	if err != nil {
		return false, err
	}
	job.Name = jobName
	job.Spec.TTLSecondsAfterFinished = nil // retain proof until BatchSandbox GC
	job.Spec.BackoffLimit = ptrToInt32(2)
	job.Spec.Template.Spec.Containers[0].Args = []string{"kata-vmstate", "prepare", "--snapshot-name", state.SnapshotName}
	if err := ctrl.SetControllerReference(bs, job, r.Scheme); err != nil {
		return false, err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, err
	}
	return false, nil
}

// When the source node has disappeared, remote cleanup must still run. This
// Job has no host mounts and uses the same operator-owned workload identity.
func (r *SandboxSnapshotReconciler) ensureRemoteOnlyCleanup(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (bool, ctrl.Result, error) {
	name := r.getKataCleanupJobName(snapshot) + "-blob"
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: name}, job)
	if err == nil {
		if job.Status.Succeeded > 0 {
			return true, ctrl.Result{}, nil
		}
		if failed := findJobCondition(job.Status.Conditions, batchv1.JobFailed); failed != nil {
			return false, ctrl.Result{}, fmt.Errorf("Blob cleanup failed: %s", failed.Message)
		}
		return false, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, ctrl.Result{}, err
	}
	job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: snapshot.Namespace}, Spec: batchv1.JobSpec{
		BackoffLimit: ptrToInt32(2), ActiveDeadlineSeconds: ptrToInt64(int64(r.getCommitJobTimeout().Seconds())),
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{
			Name: commitJobContainerName, Image: r.imageCommitterImage(), Command: []string{"/usr/local/bin/image-committer"},
			Args: []string{"kata-vmstate", "delete-remote", "--snapshot-name", snapshot.Status.KataVMState.SnapshotName}, Env: kataBlobEnv(snapshot.Status.KataVMState),
		}}}},
	}}
	if err := r.applyImageCommitterPodTemplate(&job.Spec.Template); err != nil {
		return false, ctrl.Result{}, err
	}
	if err := ctrl.SetControllerReference(snapshot, job, r.Scheme); err != nil {
		return false, ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, ctrl.Result{}, err
	}
	return false, ctrl.Result{RequeueAfter: time.Second}, nil
}
