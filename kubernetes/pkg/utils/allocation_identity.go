// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

const (
	AnnotationAllocationMode     = "sandbox.opensandbox.io/allocation-mode"
	AnnotationAllocationIdentity = "sandbox.opensandbox.io/alloc-identity"
	AnnotationAllocationIntent   = "sandbox.opensandbox.io/alloc-intent"
	AnnotationAllocationStatus   = "sandbox.opensandbox.io/alloc-status"
	AllocationModeUIDBoundV1     = "uid-bound-v1"
)

type ObjectIdentity struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// AllocationIdentity is also persisted on the Pod as a durable reservation.
// All three objects belong to the namespace of the annotation's object.
type AllocationIdentity struct {
	Version      string         `json:"version"`
	BatchSandbox ObjectIdentity `json:"batchSandbox"`
	Pool         ObjectIdentity `json:"pool"`
	Pod          ObjectIdentity `json:"pod"`
}

// AllocationMode leaves markerless native objects unchanged, but never treats
// reserved identity evidence or an invalid marker as permission to fall back.
func AllocationMode(obj metav1.Object) (bool, error) {
	mode, marked := obj.GetAnnotations()[AnnotationAllocationMode]
	_, bound := obj.GetAnnotations()[AnnotationAllocationIdentity]
	_, pending := obj.GetAnnotations()[AnnotationAllocationIntent]
	if !marked && !bound && !pending {
		return false, nil
	}
	if !marked || mode != AllocationModeUIDBoundV1 {
		return false, fmt.Errorf("unsupported or missing allocation mode %q", mode)
	}
	return true, nil
}

func ParseAllocationIdentity(raw string) (*AllocationIdentity, error) {
	var identity AllocationIdentity
	if err := decodeIdentityJSON(raw, &identity); err != nil {
		return nil, fmt.Errorf("invalid allocation identity: %w", err)
	}
	if identity.Version != AllocationModeUIDBoundV1 ||
		identity.BatchSandbox.Name == "" || identity.BatchSandbox.UID == "" ||
		identity.Pool.Name == "" || identity.Pool.UID == "" ||
		identity.Pod.Name == "" || identity.Pod.UID == "" {
		return nil, fmt.Errorf("incomplete or unsupported allocation identity")
	}
	return &identity, nil
}

// Reject duplicate fields as well as unknown fields and trailing JSON; identity
// evidence must have a single unambiguous interpretation across consumers.
func decodeIdentityJSON(raw string, value any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate or invalid field %v", key)
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	decoder = json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func ValidateUIDBoundPool(pool *sandboxv1alpha1.Pool) error {
	bound, err := AllocationMode(pool)
	if err != nil {
		return err
	}
	if !bound || pool.UID == "" {
		return fmt.Errorf("UID-bound allocation requires a marked Pool with UID")
	}
	if pool.Spec.RecycleStrategy != nil && pool.Spec.RecycleStrategy.Type != "" &&
		pool.Spec.RecycleStrategy.Type != sandboxv1alpha1.RecycleTypeDelete {
		return fmt.Errorf("UID-bound Pool requires Delete recycle")
	}
	return nil
}

func ValidateUIDBoundBatch(bs *sandboxv1alpha1.BatchSandbox) error {
	bound, err := AllocationMode(bs)
	if err != nil {
		return err
	}
	if !bound || bs.UID == "" || bs.Spec.PoolRef == "" || bs.Spec.PoolRef == "*" ||
		bs.Spec.Template != nil || (bs.Spec.Replicas != nil && *bs.Spec.Replicas != 1) {
		return fmt.Errorf("UID-bound allocation requires a marked single-replica pooled BatchSandbox with UID")
	}
	if (bs.Spec.Pause != nil && *bs.Spec.Pause) || bs.Status.Phase == sandboxv1alpha1.BatchSandboxPhasePausing ||
		bs.Status.Phase == sandboxv1alpha1.BatchSandboxPhasePaused || bs.Status.Phase == sandboxv1alpha1.BatchSandboxPhaseResuming {
		return fmt.Errorf("UID-bound allocation does not support pause/resume")
	}
	return nil
}

// BatchAllocationIdentity validates the static contract, including alloc-status.
// It does not establish that any referenced object still exists.
func BatchAllocationIdentity(bs *sandboxv1alpha1.BatchSandbox) (*AllocationIdentity, error) {
	if err := ValidateUIDBoundBatch(bs); err != nil {
		return nil, err
	}
	identity, err := ParseAllocationIdentity(bs.Annotations[AnnotationAllocationIdentity])
	if err != nil {
		return nil, err
	}
	if identity.BatchSandbox != (ObjectIdentity{Name: bs.Name, UID: bs.UID}) || identity.Pool.Name != bs.Spec.PoolRef {
		return nil, fmt.Errorf("allocation identity does not match BatchSandbox")
	}
	intent, err := ParseAllocationIdentity(bs.Annotations[AnnotationAllocationIntent])
	if err != nil || intent == nil || *intent != *identity {
		return nil, fmt.Errorf("allocation identity requires its matching durable intent")
	}
	var status struct {
		Pods       []string `json:"pods"`
		PoolRef    string   `json:"poolRef"`
		Generation int64    `json:"generation"`
	}
	if err := decodeIdentityJSON(bs.Annotations[AnnotationAllocationStatus], &status); err != nil {
		return nil, fmt.Errorf("invalid bound alloc-status: %w", err)
	}
	if len(status.Pods) != 1 || status.Pods[0] != identity.Pod.Name || status.PoolRef != identity.Pool.Name {
		return nil, fmt.Errorf("alloc-status does not match allocation identity")
	}
	return identity, nil
}

func ValidateIdentityPool(identity *AllocationIdentity, namespace string, pool *sandboxv1alpha1.Pool) error {
	if err := ValidateUIDBoundPool(pool); err != nil {
		return err
	}
	if pool.Namespace != namespace || identity.Pool != (ObjectIdentity{Name: pool.Name, UID: pool.UID}) {
		return fmt.Errorf("allocation Pool identity changed")
	}
	return nil
}

func ValidateIdentityPod(identity *AllocationIdentity, namespace string, pod *corev1.Pod) error {
	marked, err := AllocationMode(pod)
	if err != nil {
		return err
	}
	if !marked || pod.Namespace != namespace || identity.Pod != (ObjectIdentity{Name: pod.Name, UID: pod.UID}) {
		return fmt.Errorf("allocation Pod identity changed")
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != sandboxv1alpha1.GroupVersion.String() ||
		owner.Kind != "Pool" || owner.Name != identity.Pool.Name || owner.UID != identity.Pool.UID {
		return fmt.Errorf("allocation Pod is not controlled by the pinned Pool")
	}
	return nil
}

// ReadUIDBoundAllocation must receive an uncached reader. It compares live
// objects against the original binding, never reconstructing a binding by name.
func ReadUIDBoundAllocation(ctx context.Context, reader client.Reader, bs *sandboxv1alpha1.BatchSandbox) (*corev1.Pod, error) {
	identity, err := BatchAllocationIdentity(bs)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("UID-bound allocation requires an uncached reader")
	}
	live := &sandboxv1alpha1.BatchSandbox{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(bs), live); err != nil {
		return nil, err
	}
	liveIdentity, err := BatchAllocationIdentity(live)
	if err != nil {
		return nil, err
	}
	if *identity != *liveIdentity || !live.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("allocation BatchSandbox identity changed or is terminating")
	}
	for _, key := range []string{"sandbox.opensandbox.io/alloc-release", "sandbox.opensandbox.io/alloc-released"} {
		if raw, exists := live.Annotations[key]; exists {
			var release struct {
				Pods []string `json:"pods"`
			}
			if err := decodeIdentityJSON(raw, &release); err != nil || len(release.Pods) != 0 {
				return nil, fmt.Errorf("bound allocation is releasing, released, or has invalid release state")
			}
		}
	}
	pool := &sandboxv1alpha1.Pool{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: bs.Namespace, Name: identity.Pool.Name}, pool); err != nil {
		return nil, err
	}
	if err := ValidateIdentityPool(identity, bs.Namespace, pool); err != nil {
		return nil, err
	}
	if !pool.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("allocation Pool is terminating")
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: bs.Namespace, Name: identity.Pod.Name}, pod); err != nil {
		return nil, err
	}
	if err := ValidateIdentityPod(identity, bs.Namespace, pod); err != nil {
		return nil, err
	}
	reservation, err := ParseAllocationIdentity(pod.Annotations[AnnotationAllocationIdentity])
	if err != nil {
		return nil, err
	}
	if *reservation != *identity || !pod.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("allocation Pod reservation changed or is terminating")
	}
	return pod, nil
}
