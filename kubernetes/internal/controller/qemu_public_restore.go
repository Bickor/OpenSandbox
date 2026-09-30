package controller

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	snapshotcontract "github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
)

// Prepare the same restore template used by pause/resume before publishing Ready.
// Keeping the template in an immutable Secret avoids exposing credentials in status.
func (r *SandboxSnapshotReconciler) prepareQEMUPublicRestore(ctx context.Context, snapshot *sandboxv1alpha1.SandboxSnapshot) (string, error) {
	secret := &corev1.Secret{}
	name := snapshot.Name + "-source" + kataRestorePlanSecretSuffix
	if err := r.Get(ctx, types.NamespacedName{Namespace: snapshot.Namespace, Name: name}, secret); err != nil {
		return "", err
	}
	raw := secret.Data[kataRestorePlanSecretKey]
	if err := validateKataRestorePlanSecret(snapshot, secret, raw); err != nil {
		return "", err
	}
	var template corev1.PodTemplateSpec
	if err := json.Unmarshal(raw, &template); err != nil {
		return "", err
	}
	images := map[string]string{}
	for _, container := range snapshot.Status.Containers {
		image, err := snapshotcontract.ImmutableImageReference(container.ImageURI, container.ImageDigest)
		if err != nil {
			return "", err
		}
		images[container.ContainerName] = image
	}
	for i := range template.Spec.Containers {
		image, ok := images[template.Spec.Containers[i].Name]
		if !ok {
			return "", fmt.Errorf("missing snapshot for container %q", template.Spec.Containers[i].Name)
		}
		template.Spec.Containers[i].Image = image
	}
	if err := injectQEMURestore(&template, snapshot); err != nil {
		return "", err
	}
	ensureImagePullSecret(&template, r.ResumePullSecret)
	// v1 deliberately uses the proven source CPU profile, even though OCI
	// artifacts themselves are durable and can later support compatible nodes.
	template.Spec.NodeName = snapshot.Status.SourceNodeName
	prepared, err := json.Marshal(template)
	if err != nil {
		return "", err
	}
	return r.ensureKataRestorePlanSecret(ctx, snapshot, snapshot.Name+"-qemu", prepared)
}
