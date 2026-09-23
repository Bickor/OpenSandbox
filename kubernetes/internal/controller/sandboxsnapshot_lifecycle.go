// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	snapshotcontract "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

// handlePending resolves the source Pod and creates the commit Job.
func (r *SandboxSnapshotReconciler) handlePending(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if snapshot.Spec.Format == "" && r.SnapshotRegistry == "" {
		msg := "snapshot-registry not configured in controller manager"
		log.Error(nil, msg)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "RegistryNotConfigured", msg)
		return ctrl.Result{}, nil
	}

	bs := &sandboxv1alpha1.BatchSandbox{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      snapshot.Spec.SandboxName,
		Namespace: snapshot.Namespace,
	}, bs); err != nil {
		msg := fmt.Sprintf("failed to get BatchSandbox %s: %v", snapshot.Spec.SandboxName, err)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "BatchSandboxLookupFailed", msg)
		return ctrl.Result{}, nil
	}

	pod, err := r.findPodForSandbox(ctx, bs, snapshot.Namespace)
	if err != nil {
		msg := fmt.Sprintf("source pod not found: %v", err)
		log.Error(err, msg)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "SourcePodNotFound", msg)
		return ctrl.Result{}, nil
	}
	workloadContract, err := snapshotcontract.ContractFromPod(pod)
	if err != nil {
		msg := fmt.Sprintf("invalid checkpoint contract: %v", err)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "InvalidCheckpointContract", msg)
		return ctrl.Result{}, nil
	}
	snapshotFormat, err := r.resolveSnapshotFormat(ctx, snapshot, pod, workloadContract)
	if err != nil {
		msg := fmt.Sprintf("incompatible snapshot format: %v", err)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "IncompatibleSnapshotFormat", msg)
		return ctrl.Result{}, nil
	}
	if snapshotFormat != sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1 && r.SnapshotRegistry == "" {
		msg := "snapshot-registry not configured in controller manager"
		log.Error(nil, msg)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "RegistryNotConfigured", msg)
		return ctrl.Result{}, nil
	}

	sourcePodName := pod.Name
	sourceNodeName := pod.Spec.NodeName
	if sourceNodeName == "" {
		msg := "source pod is not assigned to a node"
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "SourceNodeNotFound", msg)
		return ctrl.Result{}, nil
	}

	var containers []sandboxv1alpha1.ContainerSnapshot
	var kataVMState *sandboxv1alpha1.KataVMStateSnapshot
	if snapshotFormat == sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1 {
		snapshotName, err := kataSnapshotName(snapshot.UID)
		if err != nil {
			_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "InvalidSnapshotIdentity", err.Error())
			return ctrl.Result{}, nil
		}
		podTemplate, err := kataPodTemplate(pod)
		if err != nil {
			_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "CapturePodTemplateFailed", err.Error())
			return ctrl.Result{}, nil
		}
		restorePlanSecretName, err := r.ensureKataRestorePlanSecret(ctx, snapshot, snapshotName, podTemplate.Raw)
		if err != nil {
			_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "PersistRestorePlanFailed", err.Error())
			return ctrl.Result{}, nil
		}
		kataVMState = &sandboxv1alpha1.KataVMStateSnapshot{
			SnapshotName:          snapshotName,
			RestorePlanSecretName: restorePlanSecretName,
		}
	} else {
		sourceContainers := pod.Spec.Containers
		if bs.Spec.Template != nil {
			sourceContainers = bs.Spec.Template.Spec.Containers
		}
		for _, c := range sourceContainers {
			imageURI := r.snapshotImageURI(snapshot, bs, c.Name)
			containers = append(containers, sandboxv1alpha1.ContainerSnapshot{
				ContainerName: c.Name,
				ImageURI:      imageURI,
			})
		}
		if len(containers) == 0 {
			msg := fmt.Sprintf("no containers found in BatchSandbox %s template", bs.Name)
			_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "NoContainers", msg)
			return ctrl.Result{}, nil
		}
	}

	if err := r.persistResolvedData(ctx, snapshot, sourcePodName, sourceNodeName, snapshotFormat, containers, kataVMState); err != nil {
		return ctrl.Result{}, err
	}
	snapshot.Status.SourcePodName = sourcePodName
	snapshot.Status.SourceNodeName = sourceNodeName
	snapshot.Status.Format = snapshotFormat
	snapshot.Status.Containers = containers
	snapshot.Status.KataVMState = kataVMState

	job, err := r.buildCommitJob(snapshot, string(pod.UID), workloadContract)
	if err != nil {
		msg := fmt.Sprintf("failed to build commit job: %v", err)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "BuildCommitJobFailed", msg)
		return ctrl.Result{}, nil
	}

	existingJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, existingJob); err == nil {
		log.Info("Commit job already exists", "job", job.Name)
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, "Committing", "Commit job already exists")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	} else if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "Failed to create commit job")
		r.Recorder.Eventf(snapshot, corev1.EventTypeWarning, "FailedCreateJob", "Failed to create commit job: %v", err)
		return ctrl.Result{}, err
	}

	log.Info("Created commit job", "job", job.Name)
	r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "CreatedJob", "Created commit job: %s", job.Name)
	_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseCommitting, "Committing", "Commit job created")

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// handleCommitting checks the commit Job status and transitions to Succeed or Failed.
func (r *SandboxSnapshotReconciler) handleCommitting(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Commit job not found, re-creating", "job", jobName)
			return r.handlePending(ctx, snapshot)
		}
		return ctrl.Result{}, err
	}

	if job.Status.Succeeded > 0 {
		log.Info("Commit job succeeded", "job", jobName)
		if err := r.updateSnapshotStatusFromSucceededCommitJob(ctx, snapshot, job); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(snapshot, corev1.EventTypeNormal, "JobSucceeded", "Commit job succeeded")
		return ctrl.Result{}, nil
	}

	if failedCond := findJobCondition(job.Status.Conditions, batchv1.JobFailed); failedCond != nil {
		message := "Commit job failed"
		if failedCond.Message != "" {
			message = failedCond.Message
		}
		log.Info("Commit job failed", "job", jobName, "message", message)
		if snapshot.Status.Format != sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1 {
			if err := r.ensureUnpauseJob(ctx, snapshot, imageCommitterEnvValue(job, "SOURCE_POD_UID")); err != nil {
				log.Error(err, "Failed to create best-effort unpause job")
			}
		}
		r.Recorder.Eventf(snapshot, corev1.EventTypeWarning, "JobFailed", "Commit job failed")
		_ = r.updateSnapshotStatus(ctx, snapshot, sandboxv1alpha1.SandboxSnapshotPhaseFailed, "CommitJobFailed", message)
		return ctrl.Result{}, nil
	}

	log.Info("Commit job still running", "job", jobName)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func findJobCondition(conditions []batchv1.JobCondition, conditionType batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType && conditions[i].Status == corev1.ConditionTrue {
			return &conditions[i]
		}
	}
	return nil
}

// handleDeletion cleans up the commit job and removes the finalizer.
func (r *SandboxSnapshotReconciler) handleDeletion(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if snapshot.Status.Format == sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1 {
		if snapshot.Status.KataVMState == nil {
			return ctrl.Result{}, fmt.Errorf("cannot delete kata-vmstate-v1 snapshot without kataVMState status")
		}
		stopped, result, err := r.ensureKataCommitJobStopped(ctx, snapshot)
		if err != nil || !stopped {
			return result, err
		}
		complete, result, err := r.ensureKataCleanup(ctx, snapshot)
		if err != nil || !complete {
			return result, err
		}
	}

	jobName := r.getJobName(snapshot)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job); err == nil {
		if deleteErr := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return ctrl.Result{}, deleteErr
		}
		log.Info("Deleted commit job", "job", jobName)
	}

	unpauseJobName := r.getUnpauseJobName(snapshot)
	unpauseJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: unpauseJobName}, unpauseJob); err == nil {
		if deleteErr := r.Delete(ctx, unpauseJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return ctrl.Result{}, deleteErr
		}
		log.Info("Deleted unpause job", "job", unpauseJobName)
	}
	if snapshot.Status.Format == sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1 {
		cleanupJob := &batchv1.Job{}
		cleanupJobName := r.getKataCleanupJobName(snapshot)
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: cleanupJobName}, cleanupJob); err == nil {
			if deleteErr := r.Delete(ctx, cleanupJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); deleteErr != nil && !errors.IsNotFound(deleteErr) {
				return ctrl.Result{}, deleteErr
			}
			log.Info("Deleted Kata cleanup job", "job", cleanupJobName)
		}
	}

	if controllerutil.ContainsFinalizer(snapshot, sandboxSnapshotFinalizer) {
		if err := utils.UpdateFinalizer(r.Client, snapshot, utils.RemoveFinalizerOpType, sandboxSnapshotFinalizer); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *SandboxSnapshotReconciler) ensureKataCommitJobStopped(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (bool, ctrl.Result, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: r.getJobName(snapshot)}, job)
	if errors.IsNotFound(err) {
		return true, ctrl.Result{}, nil
	}
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !errors.IsNotFound(err) {
		return false, ctrl.Result{}, err
	}
	return false, ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *SandboxSnapshotReconciler) resolveSnapshotFormat(
	ctx context.Context,
	snapshot *sandboxv1alpha1.SandboxSnapshot,
	pod *corev1.Pod,
	workloadContract snapshotcontract.WorkloadContract,
) (sandboxv1alpha1.SandboxSnapshotFormat, error) {
	requested := snapshot.Spec.Format
	if requested == "" {
		if workloadContract.Provider == snapshotcontract.ProviderQEMU {
			return sandboxv1alpha1.SandboxSnapshotFormatQEMUV1, nil
		}
		return sandboxv1alpha1.SandboxSnapshotFormatRootfsV1, nil
	}
	switch requested {
	case sandboxv1alpha1.SandboxSnapshotFormatRootfsV1:
		if workloadContract.Provider != snapshotcontract.ProviderRootfs {
			return "", fmt.Errorf("rootfs-v1 cannot snapshot checkpoint provider %q", workloadContract.Provider)
		}
	case sandboxv1alpha1.SandboxSnapshotFormatQEMUV1:
		if workloadContract.Provider != snapshotcontract.ProviderQEMU {
			return "", fmt.Errorf("qemu-v1 requires checkpoint provider %q", snapshotcontract.ProviderQEMU)
		}
	case sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1:
		if !r.KataVMStateEnabled {
			return "", fmt.Errorf("kata-vmstate-v1 is disabled")
		}
		if hasBatchSandboxControllerOwner(snapshot) {
			return "", fmt.Errorf("kata-vmstate-v1 is only valid for public snapshots")
		}
		if workloadContract.Provider != snapshotcontract.ProviderRootfs {
			return "", fmt.Errorf("kata-vmstate-v1 cannot snapshot checkpoint provider %q", workloadContract.Provider)
		}
		if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != kataRuntimeClassName {
			return "", fmt.Errorf("kata-vmstate-v1 requires runtimeClassName %q", kataRuntimeClassName)
		}
		if pod.Annotations[kataUnsupportedSecureAccessAnnotation] != "" {
			return "", fmt.Errorf("kata-vmstate-v1 does not yet support secure-access-enabled sandboxes")
		}
		if pod.Annotations[kataUnsupportedEgressAuthAnnotation] != "" {
			return "", fmt.Errorf("kata-vmstate-v1 does not yet support egress-policy-enabled sandboxes")
		}
		runtimeClass := &nodev1.RuntimeClass{}
		if err := r.Get(ctx, types.NamespacedName{Name: kataRuntimeClassName}, runtimeClass); err != nil {
			return "", fmt.Errorf("get RuntimeClass %q: %w", kataRuntimeClassName, err)
		}
		if runtimeClass.Handler != kataRuntimeHandler {
			return "", fmt.Errorf("RuntimeClass %q must use handler %q", kataRuntimeClassName, kataRuntimeHandler)
		}
	default:
		return "", fmt.Errorf("unsupported format %q", requested)
	}
	return requested, nil
}

func kataSnapshotName(uid types.UID) (string, error) {
	if uid == "" {
		return "", fmt.Errorf("SandboxSnapshot UID is required")
	}
	sum := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("ks-%x", sum[:16]), nil
}

func kataPodTemplate(pod *corev1.Pod) (runtime.RawExtension, error) {
	labels := copyPodTemplateMap(pod.Labels)
	for _, key := range []string{
		labelPoolName,
		labelPoolRevision,
		labelBatchSandboxNameKey,
		labelBatchSandboxPodIndexKey,
		labelSandboxIdentity,
		labelSandboxSnapshotID,
		labelSourceSandboxIdentity,
		labelSandboxSnapshotName,
	} {
		delete(labels, key)
	}
	annotations := copyPodTemplateMap(pod.Annotations)
	delete(annotations, kataSnapshotAnnotation)
	spec := *pod.Spec.DeepCopy()
	spec.NodeName = ""
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
		Spec:       spec,
	}
	raw, err := json.Marshal(template)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("marshal source Pod template: %w", err)
	}
	return runtime.RawExtension{Raw: raw}, nil
}

func (r *SandboxSnapshotReconciler) ensureKataRestorePlanSecret(
	ctx context.Context,
	snapshot *sandboxv1alpha1.SandboxSnapshot,
	snapshotName string,
	podTemplate []byte,
) (string, error) {
	if len(podTemplate) == 0 {
		return "", fmt.Errorf("captured Pod template is empty")
	}
	secretName := snapshotName + kataRestorePlanSecretSuffix
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: secretName}, existing)
	if err == nil {
		return secretName, validateKataRestorePlanSecret(snapshot, existing, podTemplate)
	}
	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("get Kata restore plan Secret %q: %w", secretName, err)
	}
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: snapshot.Namespace,
			Labels:    map[string]string{labelSandboxSnapshotName: snapshot.Name},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{kataRestorePlanSecretKey: append([]byte(nil), podTemplate...)},
	}
	if err := ctrl.SetControllerReference(snapshot, secret, r.Scheme); err != nil {
		return "", fmt.Errorf("set Kata restore plan Secret owner: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create Kata restore plan Secret %q: %w", secretName, err)
		}
		raced := &corev1.Secret{}
		if getErr := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: secretName}, raced); getErr != nil {
			return "", fmt.Errorf("read raced Kata restore plan Secret %q: %w", secretName, getErr)
		}
		if validateErr := validateKataRestorePlanSecret(snapshot, raced, podTemplate); validateErr != nil {
			return "", validateErr
		}
	}
	return secretName, nil
}

func validateKataRestorePlanSecret(snapshot *sandboxv1alpha1.SandboxSnapshot, secret *corev1.Secret, podTemplate []byte) error {
	if secret == nil || secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable {
		return fmt.Errorf("Kata restore plan Secret must be immutable and opaque")
	}
	if !bytes.Equal(secret.Data[kataRestorePlanSecretKey], podTemplate) || len(secret.Data) != 1 {
		return fmt.Errorf("Kata restore plan Secret %q does not match the captured Pod template", secret.Name)
	}
	for _, owner := range secret.OwnerReferences {
		if owner.APIVersion == sandboxv1alpha1.GroupVersion.String() && owner.Kind == "SandboxSnapshot" && owner.Name == snapshot.Name && owner.UID == snapshot.UID && owner.Controller != nil && *owner.Controller {
			return nil
		}
	}
	return fmt.Errorf("Kata restore plan Secret %q is not controlled by SandboxSnapshot %s/%s", secret.Name, snapshot.Namespace, snapshot.Name)
}

// findPodForSandbox finds the running pod belonging to a BatchSandbox.
func (r *SandboxSnapshotReconciler) findPodForSandbox(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, namespace string) (*corev1.Pod, error) {
	alloc, err := parseSandboxAllocation(bs)
	if err == nil && len(alloc.Pods) > 0 {
		for _, podName := range alloc.Pods {
			pod := &corev1.Pod{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err == nil {
				if pod.Status.Phase == corev1.PodRunning {
					return pod, nil
				}
			}
		}
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{labelBatchSandboxNameKey: bs.Name},
	); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	for i := range podList.Items {
		if podList.Items[i].Status.Phase == corev1.PodRunning {
			return &podList.Items[i], nil
		}
	}

	podName := fmt.Sprintf("%s-%d", bs.Name, batchSandboxFirstPodIndex)
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err == nil {
		if pod.Status.Phase == corev1.PodRunning {
			return pod, nil
		}
	}

	return nil, fmt.Errorf("no running pod found for BatchSandbox %s", bs.Name)
}

func (r *SandboxSnapshotReconciler) snapshotImageURI(
	snapshot *sandboxv1alpha1.SandboxSnapshot,
	bs *sandboxv1alpha1.BatchSandbox,
	containerName string,
) string {
	return fmt.Sprintf(
		"%s/%s-%s:%s",
		r.SnapshotRegistry,
		bs.Name,
		containerName,
		snapshotImageTag(snapshot, bs),
	)
}

func (r *SandboxSnapshotReconciler) vmStateImageURI(snapshot *sandboxv1alpha1.SandboxSnapshot) (string, error) {
	if len(snapshot.Status.Containers) == 0 {
		return "", fmt.Errorf("snapshot has no resolved container image URI")
	}
	rootfsImage := snapshot.Status.Containers[0].ImageURI
	lastSlash := strings.LastIndexByte(rootfsImage, '/')
	lastColon := strings.LastIndexByte(rootfsImage, ':')
	if lastColon <= lastSlash || lastColon == len(rootfsImage)-1 {
		return "", fmt.Errorf("snapshot container image %q has no tag", rootfsImage)
	}
	return fmt.Sprintf("%s/%s-vmstate:%s", r.SnapshotRegistry, snapshot.Spec.SandboxName, rootfsImage[lastColon+1:]), nil
}

func snapshotImageTag(snapshot *sandboxv1alpha1.SandboxSnapshot, bs *sandboxv1alpha1.BatchSandbox) string {
	if hasBatchSandboxControllerOwner(snapshot) {
		return fmt.Sprintf("snap-gen%d", bs.Generation)
	}
	return publicSnapshotImageTag(snapshot.Name)
}

func hasBatchSandboxControllerOwner(snapshot *sandboxv1alpha1.SandboxSnapshot) bool {
	for _, owner := range snapshot.OwnerReferences {
		if owner.Kind != "BatchSandbox" {
			continue
		}
		if owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func publicSnapshotImageTag(snapshotName string) string {
	const publicSnapshotNamePrefix = "osb-snap-"
	if strings.HasPrefix(snapshotName, publicSnapshotNamePrefix) {
		suffix := strings.TrimPrefix(snapshotName, publicSnapshotNamePrefix)
		if isLowerHex(suffix) && len(suffix) == 32 {
			return "snap-" + suffix
		}
	}

	sum := sha256.Sum256([]byte(snapshotName))
	return fmt.Sprintf("snap-%x", sum)[:37]
}

func isLowerHex(value string) bool {
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}

func (r *SandboxSnapshotReconciler) imageCommitterImage() string {
	if r.ImageCommitterImage != "" {
		return r.ImageCommitterImage
	}
	return "image-committer:dev"
}

func (r *SandboxSnapshotReconciler) containerdSocketPath() string {
	if r.ContainerdSocketPath != "" {
		return r.ContainerdSocketPath
	}
	return ContainerdSocketPath
}

func (r *SandboxSnapshotReconciler) imageCommitterPullSecrets() []corev1.LocalObjectReference {
	if r.ImageCommitterPullSecret == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: r.ImageCommitterPullSecret}}
}

func commitJobSecurityContext(requiresHostPID bool) *corev1.SecurityContext {
	securityContext := &corev1.SecurityContext{
		RunAsUser:                ptrToInt64(0),
		RunAsNonRoot:             ptrToBool(false),
		AllowPrivilegeEscalation: ptrToBool(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
	if requiresHostPID {
		securityContext.Capabilities.Add = []corev1.Capability{"SYS_PTRACE"}
	}
	return securityContext
}

func (r *SandboxSnapshotReconciler) buildCommitJob(snapshot *sandboxv1alpha1.SandboxSnapshot, sourcePodUID string, contracts ...snapshotcontract.WorkloadContract) (*batchv1.Job, error) {
	if snapshot.Status.Format == sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1 {
		return r.buildKataVMStateJob(snapshot, sourcePodUID, false)
	}
	jobName := r.getJobName(snapshot)
	imageCommitterImage := r.imageCommitterImage()

	fifoDirType := corev1.HostPathDirectoryOrCreate
	volumeMounts := []corev1.VolumeMount{
		{Name: "containerd-sock", MountPath: ContainerdSocketPath},
		{Name: "containerd-fifo", MountPath: containerdFIFODir},
	}
	volumes := []corev1.Volume{
		{
			Name: "containerd-sock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: r.containerdSocketPath()},
			},
		},
		{
			Name: "containerd-fifo",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: containerdFIFODir, Type: &fifoDirType},
			},
		},
	}
	workloadContract := snapshotcontract.WorkloadContract{Provider: snapshotcontract.ProviderRootfs}
	if len(contracts) > 0 {
		workloadContract = contracts[0]
	}
	requiresHostPID := workloadContract.Provider == snapshotcontract.ProviderQEMU
	if requiresHostPID {
		// nerdctl exec exchanges stdio with containerd-shim through FIFOs beside
		// the socket. The QEMU worker therefore needs the runtime directory at
		// the same path, while rootfs-only workers keep the narrower socket mount.
		volumeMounts[0].MountPath = filepath.Dir(ContainerdSocketPath)
		volumes[0].HostPath.Path = filepath.Dir(r.containerdSocketPath())
	}
	if snapshot.Status.Format == sandboxv1alpha1.SandboxSnapshotFormatQEMUV1 && workloadContract.Provider != snapshotcontract.ProviderQEMU {
		return nil, fmt.Errorf("qemu-v1 snapshot requires the resolved QEMU workload contract")
	}

	if r.SnapshotPushSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "registry-creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: r.SnapshotPushSecret,
					Items: []corev1.KeyToPath{
						{Key: ".dockerconfigjson", Path: "config.json"},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name: "registry-creds", MountPath: "/var/run/opensandbox/registry", ReadOnly: true,
		})
	}

	var containerSpecs []string
	for _, cs := range snapshot.Status.Containers {
		containerSpecs = append(containerSpecs, fmt.Sprintf("%s:%s", cs.ContainerName, cs.ImageURI))
	}
	args := append([]string{snapshot.Status.SourcePodName, snapshot.Namespace}, containerSpecs...)
	env := []corev1.EnvVar{{Name: "CONTAINERD_SOCKET", Value: ContainerdSocketPath}}
	if sourcePodUID != "" {
		env = append(env, corev1.EnvVar{Name: "SOURCE_POD_UID", Value: sourcePodUID})
	}
	resources := corev1.ResourceRequirements{}
	if workloadContract.Provider == snapshotcontract.ProviderQEMU {
		vmStateImageURI, err := r.vmStateImageURI(snapshot)
		if err != nil {
			return nil, err
		}
		request := snapshotcontract.Request{
			Version:           snapshotcontract.RequestVersionV1,
			PodName:           snapshot.Status.SourcePodName,
			PodUID:            sourcePodUID,
			Namespace:         snapshot.Namespace,
			Provider:          workloadContract.Provider,
			Containers:        make([]snapshotcontract.ContainerTarget, 0, len(snapshot.Status.Containers)),
			VMStateImageURI:   vmStateImageURI,
			LeaveSourceFrozen: hasBatchSandboxControllerOwner(snapshot),
			QEMU: &snapshotcontract.QEMURequest{
				ContainerName:      workloadContract.QEMU.ContainerName,
				QMPSocketPath:      workloadContract.QEMU.QMPSocketPath,
				LaunchManifestPath: workloadContract.QEMU.LaunchManifestPath,
				RequiredNodeClass:  workloadContract.QEMU.RequiredNodeClass,
				VolumeMountPaths:   workloadContract.QEMU.VolumeMountPaths,
			},
		}
		for _, container := range snapshot.Status.Containers {
			request.Containers = append(request.Containers, snapshotcontract.ContainerTarget{Name: container.ContainerName, ImageURI: container.ImageURI})
		}
		requestData, err := json.Marshal(request)
		if err != nil {
			return nil, fmt.Errorf("marshal QEMU snapshot request: %w", err)
		}
		args = []string{"snapshot", "--request-base64", base64.StdEncoding.EncodeToString(requestData)}
		volumes = append(volumes, corev1.Volume{
			Name: "vmstate-work",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: resource.NewQuantity(64<<30, resource.BinarySI),
			}},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "vmstate-work", MountPath: "/workspace/checkpoint"})
		env = append(env,
			corev1.EnvVar{Name: "SNAPSHOT_VMSTATE_WORK_DIR", Value: "/workspace/checkpoint"},
			corev1.EnvVar{Name: "SNAPSHOT_VMSTATE_MAX_BYTES", Value: fmt.Sprintf("%d", int64(32<<30))},
		)
		resources.Requests = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")}
		resources.Limits = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("64Gi")}
	}
	if r.SnapshotRegistryInsecure {
		env = append(env, corev1.EnvVar{Name: "SNAPSHOT_REGISTRY_INSECURE", Value: "true"})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: snapshot.Namespace,
			Labels: map[string]string{
				labelSandboxSnapshotName:  snapshot.Name,
				labelPrivilegedNodeAccess: "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrToInt32(DefaultCommitJobBackoffLimit),
			TTLSecondsAfterFinished: ptrToInt32(int32(defaultTTLSecondsAfterFinished)),
			ActiveDeadlineSeconds:   ptrToInt64(int64(r.getCommitJobTimeout().Seconds())),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					HostPID:          requiresHostPID,
					ImagePullSecrets: r.imageCommitterPullSecrets(),
					Containers: []corev1.Container{
						{
							Name:            commitJobContainerName,
							Image:           imageCommitterImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/usr/local/bin/image-committer"},
							Args:            args,
							VolumeMounts:    volumeMounts,
							Env:             env,
							Resources:       resources,
							SecurityContext: commitJobSecurityContext(requiresHostPID),
						},
					},
					Volumes:  volumes,
					NodeName: snapshot.Status.SourceNodeName,
				},
			},
		},
	}

	if err := r.applyImageCommitterPodTemplate(&job.Spec.Template); err != nil {
		return nil, err
	}
	if err := ctrl.SetControllerReference(snapshot, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}
	return job, nil
}

func (r *SandboxSnapshotReconciler) buildKataVMStateJob(snapshot *sandboxv1alpha1.SandboxSnapshot, sourcePodUID string, cleanup bool) (*batchv1.Job, error) {
	if snapshot.Status.KataVMState == nil {
		return nil, fmt.Errorf("kata-vmstate-v1 snapshot status is missing kataVMState")
	}
	if snapshot.Status.SourcePodName == "" && !cleanup {
		return nil, fmt.Errorf("kata-vmstate-v1 snapshot status is missing sourcePodName")
	}
	if snapshot.Status.SourceNodeName == "" {
		return nil, fmt.Errorf("kata-vmstate-v1 snapshot status is missing sourceNodeName")
	}
	snapshotName := snapshot.Status.KataVMState.SnapshotName
	if !isValidKataSnapshotName(snapshotName) {
		return nil, fmt.Errorf("invalid Kata snapshot name %q", snapshotName)
	}
	if !filepath.IsAbs(r.hostKataCtlPath()) || filepath.Clean(r.hostKataCtlPath()) != r.hostKataCtlPath() || r.hostKataCtlPath() == "/" {
		return nil, fmt.Errorf("host kata-ctl path must be a clean absolute path")
	}
	if sourcePodUID == "" && !cleanup {
		return nil, fmt.Errorf("kata-vmstate-v1 snapshot requires the source Pod UID")
	}

	jobName := r.getJobName(snapshot)
	args := []string{
		"kata-vmstate", "create",
		"--pod-name", snapshot.Status.SourcePodName,
		"--pod-namespace", snapshot.Namespace,
		"--pod-uid", sourcePodUID,
		"--snapshot-name", snapshotName,
		"--kata-ctl-path", r.hostKataCtlPath(),
	}
	volumes := []corev1.Volume{{
		Name: kataHostRootVolumeName,
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: "/",
		}},
	}}
	hostToContainer := corev1.MountPropagationHostToContainer
	volumeMounts := []corev1.VolumeMount{{
		Name:             kataHostRootVolumeName,
		MountPath:        kataHostRootMountPath,
		MountPropagation: &hostToContainer,
	}}
	env := []corev1.EnvVar(nil)
	if cleanup {
		jobName = r.getKataCleanupJobName(snapshot)
		args = []string{"kata-vmstate", "delete", "--snapshot-name", snapshotName}
	} else {
		volumes = append(volumes, corev1.Volume{
			Name: "containerd-sock",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: r.containerdSocketPath(),
			}},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "containerd-sock", MountPath: ContainerdSocketPath})
		env = append(env, corev1.EnvVar{Name: "CONTAINERD_SOCKET", Value: ContainerdSocketPath})
	}
	privileged := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: snapshot.Namespace,
			Labels: map[string]string{
				labelSandboxSnapshotName:  snapshot.Name,
				labelPrivilegedNodeAccess: "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrToInt32(DefaultCommitJobBackoffLimit),
			TTLSecondsAfterFinished: ptrToInt32(int32(defaultTTLSecondsAfterFinished)),
			ActiveDeadlineSeconds:   ptrToInt64(int64(r.getCommitJobTimeout().Seconds())),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy:    corev1.RestartPolicyNever,
				ImagePullSecrets: r.imageCommitterPullSecrets(),
				Containers: []corev1.Container{{
					Name:            commitJobContainerName,
					Image:           r.imageCommitterImage(),
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"/usr/local/bin/image-committer"},
					Args:            args,
					Env:             env,
					VolumeMounts:    volumeMounts,
					SecurityContext: &corev1.SecurityContext{
						RunAsUser:                ptrToInt64(0),
						RunAsNonRoot:             ptrToBool(false),
						Privileged:               &privileged,
						AllowPrivilegeEscalation: ptrToBool(true),
					},
				}},
				Volumes:  volumes,
				NodeName: snapshot.Status.SourceNodeName,
			}},
		},
	}
	if err := r.applyImageCommitterPodTemplate(&job.Spec.Template); err != nil {
		return nil, err
	}
	if !cleanup {
		if err := ctrl.SetControllerReference(snapshot, job, r.Scheme); err != nil {
			return nil, fmt.Errorf("failed to set controller reference: %w", err)
		}
	}
	return job, nil
}

func isValidKataSnapshotName(name string) bool {
	if len(name) != 35 || !strings.HasPrefix(name, "ks-") {
		return false
	}
	return isLowerHex(strings.TrimPrefix(name, "ks-"))
}

func (r *SandboxSnapshotReconciler) hostKataCtlPath() string {
	if r.HostKataCtlPath != "" {
		return r.HostKataCtlPath
	}
	return defaultHostKataCtlPath
}

func (r *SandboxSnapshotReconciler) applyImageCommitterPodTemplate(generated *corev1.PodTemplateSpec) error {
	if generated == nil {
		return fmt.Errorf("generated image-committer Pod template is required")
	}

	var overlay *corev1.PodTemplateSpec
	if r.ImageCommitterPodTemplate != nil {
		overlay = r.ImageCommitterPodTemplate.DeepCopy()
	} else {
		overlay = &corev1.PodTemplateSpec{}
	}

	generated.Labels = mergeStringMaps(overlay.Labels, generated.Labels)
	generated.Annotations = mergeStringMaps(overlay.Annotations, generated.Annotations)

	generatedContainer := generated.Spec.Containers[0]
	commitContainer := corev1.Container{Name: commitJobContainerName}
	commitCount := 0
	containers := make([]corev1.Container, 0, len(overlay.Spec.Containers)+1)
	for _, container := range overlay.Spec.Containers {
		if container.Name != commitJobContainerName {
			containers = append(containers, container)
			continue
		}
		commitCount++
		commitContainer = container
	}
	if commitCount > 1 {
		return fmt.Errorf("image-committer Pod template contains multiple %q containers", commitJobContainerName)
	}

	commitContainer.Name = generatedContainer.Name
	commitContainer.Image = generatedContainer.Image
	commitContainer.ImagePullPolicy = generatedContainer.ImagePullPolicy
	commitContainer.Command = generatedContainer.Command
	commitContainer.Args = generatedContainer.Args
	commitContainer.Env = mergeEnvVars(commitContainer.Env, generatedContainer.Env)
	commitContainer.VolumeMounts = mergeVolumeMounts(commitContainer.VolumeMounts, generatedContainer.VolumeMounts)
	commitContainer.Resources = mergeResourceRequirements(commitContainer.Resources, generatedContainer.Resources)
	commitContainer.SecurityContext = generatedContainer.SecurityContext
	commitContainer.TerminationMessagePath = "/dev/termination-log"
	commitContainer.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	containers = append([]corev1.Container{commitContainer}, containers...)

	overlay.Spec.Containers = containers
	overlay.Spec.Volumes = mergeVolumes(overlay.Spec.Volumes, generated.Spec.Volumes)
	overlay.Spec.ImagePullSecrets = mergeLocalObjectReferences(overlay.Spec.ImagePullSecrets, generated.Spec.ImagePullSecrets)
	overlay.Spec.HostPID = generated.Spec.HostPID
	overlay.Spec.RestartPolicy = generated.Spec.RestartPolicy
	overlay.Spec.NodeName = generated.Spec.NodeName

	generated.Spec = overlay.Spec
	return nil
}

func mergeStringMaps(maps ...map[string]string) map[string]string {
	var result map[string]string
	for _, values := range maps {
		for key, value := range values {
			if result == nil {
				result = make(map[string]string)
			}
			result[key] = value
		}
	}
	return result
}

func mergeEnvVars(base, required []corev1.EnvVar) []corev1.EnvVar {
	result := append([]corev1.EnvVar(nil), base...)
	for _, value := range required {
		replaced := false
		for i := range result {
			if result[i].Name == value.Name {
				result[i] = value
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, value)
		}
	}
	return result
}

func mergeVolumeMounts(base, required []corev1.VolumeMount) []corev1.VolumeMount {
	result := append([]corev1.VolumeMount(nil), base...)
	for _, value := range required {
		replaced := false
		for i := range result {
			if result[i].Name == value.Name {
				result[i] = value
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, value)
		}
	}
	return result
}

func mergeResourceRequirements(base, required corev1.ResourceRequirements) corev1.ResourceRequirements {
	result := *base.DeepCopy()
	if result.Limits == nil && len(required.Limits) > 0 {
		result.Limits = corev1.ResourceList{}
	}
	for name, quantity := range required.Limits {
		result.Limits[name] = quantity.DeepCopy()
	}
	if result.Requests == nil && len(required.Requests) > 0 {
		result.Requests = corev1.ResourceList{}
	}
	for name, quantity := range required.Requests {
		result.Requests[name] = quantity.DeepCopy()
	}
	for _, claim := range required.Claims {
		replaced := false
		for i := range result.Claims {
			if result.Claims[i].Name == claim.Name {
				result.Claims[i] = claim
				replaced = true
				break
			}
		}
		if !replaced {
			result.Claims = append(result.Claims, claim)
		}
	}
	return result
}

func mergeVolumes(base, required []corev1.Volume) []corev1.Volume {
	result := append([]corev1.Volume(nil), base...)
	for _, value := range required {
		replaced := false
		for i := range result {
			if result[i].Name == value.Name {
				result[i] = value
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, value)
		}
	}
	return result
}

func mergeLocalObjectReferences(base, required []corev1.LocalObjectReference) []corev1.LocalObjectReference {
	result := append([]corev1.LocalObjectReference(nil), base...)
	for _, value := range required {
		found := false
		for _, existing := range result {
			if existing.Name == value.Name {
				found = true
				break
			}
		}
		if !found {
			result = append(result, value)
		}
	}
	return result
}

func (r *SandboxSnapshotReconciler) ensureUnpauseJob(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot, sourcePodUID string) error {
	if snapshot.Status.SourcePodName == "" || snapshot.Status.SourceNodeName == "" || len(snapshot.Status.Containers) == 0 {
		return nil
	}

	jobName := r.getUnpauseJobName(snapshot)
	existingJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, existingJob); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	workloadContract := snapshotcontract.WorkloadContract{Provider: snapshotcontract.ProviderRootfs}
	if snapshot.Status.Format == sandboxv1alpha1.SandboxSnapshotFormatQEMUV1 {
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Status.SourcePodName}, pod); err != nil {
			return fmt.Errorf("get source Pod for QEMU recovery: %w", err)
		}
		var err error
		workloadContract, err = snapshotcontract.ContractFromPod(pod)
		if err != nil {
			return fmt.Errorf("resolve QEMU recovery contract: %w", err)
		}
	}
	job, err := r.buildUnpauseJob(snapshot, workloadContract)
	if err != nil {
		return err
	}
	// Propagate the source Pod UID from the commit job so image-committer can
	// do secure container matching on unpause, consistent with the commit path.
	if sourcePodUID != "" {
		for i := range job.Spec.Template.Spec.Containers {
			if job.Spec.Template.Spec.Containers[i].Name == commitJobContainerName {
				job.Spec.Template.Spec.Containers[i].Env = append(
					job.Spec.Template.Spec.Containers[i].Env,
					corev1.EnvVar{Name: "SOURCE_POD_UID", Value: sourcePodUID},
				)
				break
			}
		}
	}
	return r.Create(ctx, job)
}

func (r *SandboxSnapshotReconciler) buildUnpauseJob(snapshot *sandboxv1alpha1.SandboxSnapshot, contracts ...snapshotcontract.WorkloadContract) (*batchv1.Job, error) {
	var containerNames []string
	for _, cs := range snapshot.Status.Containers {
		containerNames = append(containerNames, cs.ContainerName)
	}
	args := append([]string{"unpause", snapshot.Status.SourcePodName, snapshot.Namespace}, containerNames...)
	requiresHostPID := len(contracts) > 0 && contracts[0].Provider == snapshotcontract.ProviderQEMU
	containerdHostPath := r.containerdSocketPath()
	containerdMountPath := ContainerdSocketPath
	if requiresHostPID {
		containerdHostPath = filepath.Dir(containerdHostPath)
		containerdMountPath = filepath.Dir(containerdMountPath)
	}
	if requiresHostPID {
		contract := contracts[0]
		if contract.QEMU == nil {
			return nil, fmt.Errorf("QEMU recovery contract is incomplete")
		}
		args = append([]string{
			"recover-qemu",
			snapshot.Status.SourcePodName,
			snapshot.Namespace,
			contract.QEMU.ContainerName,
			contract.QEMU.QMPSocketPath,
		}, containerNames...)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.getUnpauseJobName(snapshot),
			Namespace: snapshot.Namespace,
			Labels: map[string]string{
				labelSandboxSnapshotName:  snapshot.Name,
				labelPrivilegedNodeAccess: "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrToInt32(0),
			TTLSecondsAfterFinished: ptrToInt32(int32(defaultTTLSecondsAfterFinished)),
			ActiveDeadlineSeconds:   ptrToInt64(int64(r.getCommitJobTimeout().Seconds())),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					HostPID:          requiresHostPID,
					ImagePullSecrets: r.imageCommitterPullSecrets(),
					Containers: []corev1.Container{
						{
							Name:            commitJobContainerName,
							Image:           r.imageCommitterImage(),
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/usr/local/bin/image-committer"},
							Args:            args,
							VolumeMounts: []corev1.VolumeMount{
								{Name: "containerd-sock", MountPath: containerdMountPath},
							},
							Env: []corev1.EnvVar{
								{Name: "CONTAINERD_SOCKET", Value: ContainerdSocketPath},
							},
							SecurityContext: commitJobSecurityContext(requiresHostPID),
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "containerd-sock",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: containerdHostPath},
							},
						},
					},
					NodeName: snapshot.Status.SourceNodeName,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(snapshot, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}
	return job, nil
}

type commitJobResult = snapshotcontract.Result

func (r *SandboxSnapshotReconciler) updateSnapshotStatusFromSucceededCommitJob(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot, job *batchv1.Job) error {
	result, found, err := r.getCommitJobResult(ctx, snapshot.Namespace, job.Name)
	if err != nil {
		return err
	}
	digests := map[string]string{}
	if found {
		digests = make(map[string]string, len(result.Containers))
		for _, container := range result.Containers {
			if container.Name == "" || container.Digest == "" {
				continue
			}
			digests[container.Name] = container.Digest
		}
	}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &sandboxv1alpha1.SandboxSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: snapshot.Name}, latest); err != nil {
			return err
		}
		if isTerminalSnapshotPhase(latest.Status.Phase) && latest.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseSucceed {
			return nil
		}
		for i := range latest.Status.Containers {
			if digest, ok := digests[latest.Status.Containers[i].ContainerName]; ok {
				latest.Status.Containers[i].ImageDigest = digest
			}
		}
		if latest.Status.Format == sandboxv1alpha1.SandboxSnapshotFormatQEMUV1 {
			if !found || result.VirtualMachine == nil {
				return fmt.Errorf("successful QEMU commit job did not report virtualMachine result")
			}
			vm := result.VirtualMachine
			latest.Status.VirtualMachine = &sandboxv1alpha1.VirtualMachineSnapshot{
				ImageURI:       vm.ImageURI,
				ImageDigest:    vm.ImageDigest,
				PayloadDigest:  vm.PayloadDigest,
				SizeBytes:      vm.SizeBytes,
				Compression:    vm.Compression,
				ManifestDigest: vm.ManifestDigest,
				Compatibility: sandboxv1alpha1.QEMUCompatibility{
					Architecture:      vm.Compatibility.Architecture,
					QEMUVersion:       vm.Compatibility.QEMUVersion,
					MachineType:       vm.Compatibility.MachineType,
					CPUModel:          vm.Compatibility.CPUModel,
					VCPUs:             vm.Compatibility.VCPUs,
					MemoryBytes:       vm.Compatibility.MemoryBytes,
					QEMUConfigDigest:  vm.Compatibility.QEMUConfigDigest,
					RequiredNodeClass: vm.Compatibility.RequiredNodeClass,
				},
			}
		}
		if latest.Status.Format == sandboxv1alpha1.SandboxSnapshotFormatKataVMStateV1 {
			if !found || result.KataVMState == nil {
				return fmt.Errorf("successful Kata commit job did not report kataVMState result")
			}
			if latest.Status.KataVMState == nil {
				return fmt.Errorf("kata-vmstate-v1 snapshot status has no resolved kataVMState")
			}
			if result.KataVMState.SnapshotName != latest.Status.KataVMState.SnapshotName {
				return fmt.Errorf("Kata commit job returned snapshot name %q, expected %q", result.KataVMState.SnapshotName, latest.Status.KataVMState.SnapshotName)
			}
			if result.KataVMState.RuntimeVersion == "" {
				return fmt.Errorf("Kata commit job returned an empty runtime version")
			}
			latest.Status.KataVMState.RuntimeVersion = result.KataVMState.RuntimeVersion
		}
		latest.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhaseSucceed
		applySnapshotPhaseConditions(&latest.Status, "", "")
		return r.Status().Update(ctx, latest)
	})
}

func (r *SandboxSnapshotReconciler) getCommitJobResult(ctx context.Context, namespace, jobName string) (*commitJobResult, bool, error) {
	for _, labelKey := range []string{"job-name", "batch.kubernetes.io/job-name"} {
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList,
			client.InNamespace(namespace),
			client.MatchingLabels{labelKey: jobName},
		); err != nil {
			return nil, false, err
		}
		for i := range podList.Items {
			if result, found, err := snapshotResultFromPod(&podList.Items[i]); found || err != nil {
				return result, found, err
			}
		}
	}
	return nil, false, nil
}

func snapshotResultFromPod(pod *corev1.Pod) (*commitJobResult, bool, error) {
	if pod == nil {
		return nil, false, nil
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != commitJobContainerName || status.State.Terminated == nil {
			continue
		}
		if status.State.Terminated.ExitCode != 0 {
			continue
		}
		message := strings.TrimSpace(status.State.Terminated.Message)
		if message == "" {
			return nil, false, nil
		}
		var result commitJobResult
		if err := json.Unmarshal([]byte(message), &result); err != nil {
			return nil, false, fmt.Errorf("failed to parse commit job termination message from pod %s: %w", pod.Name, err)
		}
		return &result, true, nil
	}
	return nil, false, nil
}

func imageCommitterEnvValue(job *batchv1.Job, name string) string {
	if job == nil {
		return ""
	}
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name != commitJobContainerName {
			continue
		}
		for _, env := range container.Env {
			if env.Name == name {
				return env.Value
			}
		}
	}
	return ""
}

func (r *SandboxSnapshotReconciler) getJobName(snapshot *sandboxv1alpha1.SandboxSnapshot) string {
	return fmt.Sprintf("%s-commit", snapshot.Name)
}

func (r *SandboxSnapshotReconciler) getUnpauseJobName(snapshot *sandboxv1alpha1.SandboxSnapshot) string {
	return fmt.Sprintf("%s-unpause", snapshot.Name)
}

func (r *SandboxSnapshotReconciler) getKataCleanupJobName(snapshot *sandboxv1alpha1.SandboxSnapshot) string {
	return snapshot.Name + kataCleanupJobNameSuffix
}

func (r *SandboxSnapshotReconciler) ensureKataCleanup(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (bool, ctrl.Result, error) {
	if snapshot.Status.KataVMState == nil || !isValidKataSnapshotName(snapshot.Status.KataVMState.SnapshotName) {
		return false, ctrl.Result{}, fmt.Errorf("cannot clean up invalid kataVMState status")
	}
	jobName := r.getKataCleanupJobName(snapshot)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: jobName}, job)
	jobExists := err == nil
	if err == nil {
		if job.Status.Succeeded > 0 {
			result, found, resultErr := r.getCommitJobResult(ctx, snapshot.Namespace, jobName)
			if resultErr != nil {
				return false, ctrl.Result{}, resultErr
			}
			if !found || result.KataVMState == nil || result.KataVMState.SnapshotName != snapshot.Status.KataVMState.SnapshotName {
				return false, ctrl.Result{}, fmt.Errorf("successful Kata cleanup job returned an invalid snapshot result")
			}
			return true, ctrl.Result{}, nil
		}
		nodeExists, err := r.requireReadyKataCleanupNode(ctx, snapshot.Status.SourceNodeName)
		if err != nil {
			return false, ctrl.Result{}, err
		}
		if !nodeExists {
			return true, ctrl.Result{}, nil
		}
		if failed := findJobCondition(job.Status.Conditions, batchv1.JobFailed); failed == nil {
			return false, ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	} else if !errors.IsNotFound(err) {
		return false, ctrl.Result{}, err
	}

	nodeExists, err := r.requireReadyKataCleanupNode(ctx, snapshot.Status.SourceNodeName)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if !nodeExists {
		return true, ctrl.Result{}, nil
	}
	if jobExists {
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) {
			return false, ctrl.Result{}, err
		}
		return false, ctrl.Result{RequeueAfter: time.Second}, nil
	}

	cleanupJob, err := r.buildKataVMStateJob(snapshot, "", true)
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if err := r.Create(ctx, cleanupJob); err != nil && !errors.IsAlreadyExists(err) {
		return false, ctrl.Result{}, err
	}
	return false, ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *SandboxSnapshotReconciler) requireReadyKataCleanupNode(ctx context.Context, nodeName string) (bool, error) {
	if nodeName == "" {
		return false, fmt.Errorf("source node is not recorded for Kata snapshot cleanup")
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if errors.IsNotFound(err) {
			// Artifacts are node-local. Once the source node is gone there is no
			// remaining node on which cleanup can or needs to run.
			return false, nil
		}
		return false, fmt.Errorf("source node %q unavailable for Kata snapshot cleanup: %w", nodeName, err)
	}
	if !nodeIsReady(node) {
		return false, fmt.Errorf("source node %q is not Ready for Kata snapshot cleanup", nodeName)
	}
	return true, nil
}

func nodeIsReady(node *corev1.Node) bool {
	if node == nil || !node.DeletionTimestamp.IsZero() {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
