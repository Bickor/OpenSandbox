// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	pkgutils "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/utils"
)

func hasAllocationIdentityContract(obj metav1.Object) bool {
	_, mode := obj.GetAnnotations()[pkgutils.AnnotationAllocationMode]
	_, identity := obj.GetAnnotations()[pkgutils.AnnotationAllocationIdentity]
	_, intent := obj.GetAnnotations()[pkgutils.AnnotationAllocationIntent]
	return mode || identity || intent
}

func validateAllocationModes(pool *sandboxv1alpha1.Pool, batches []*sandboxv1alpha1.BatchSandbox) (bool, error) {
	protected, err := pkgutils.AllocationMode(pool)
	if err != nil {
		return false, err
	}
	if protected {
		if err := pkgutils.ValidateUIDBoundPool(pool); err != nil {
			return false, err
		}
	}
	for _, bs := range batches {
		bound, err := pkgutils.AllocationMode(bs)
		if err != nil {
			return false, err
		}
		if protected != bound {
			return false, fmt.Errorf("Pool and BatchSandbox allocation modes must match")
		}
		if bound {
			if err := pkgutils.ValidateUIDBoundBatch(bs); err != nil {
				return false, err
			}
			if bs.Namespace != pool.Namespace || bs.Spec.PoolRef != pool.Name {
				return false, fmt.Errorf("BatchSandbox references a different Pool")
			}
		}
	}
	return protected, nil
}

// Protected pools use durable Pod reservations instead of the native name-only
// allocation store. This also recovers unpublished reservations after restart.
func (r *PoolReconciler) reconcileUIDBoundPool(ctx context.Context, pool *sandboxv1alpha1.Pool) (ctrl.Result, error) {
	if err := r.validateLiveBoundPool(ctx, pool); err != nil {
		return ctrl.Result{}, err
	}
	// Native indexing remains unchanged. Protected allocation takes an uncached
	// snapshot so an unpublished reservation cannot disappear behind cache lag.
	podList := &corev1.PodList{}
	if err := r.APIReader.List(ctx, podList, client.InNamespace(pool.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	var pods []*corev1.Pod
	var total int32
	for i := range podList.Items {
		pod := &podList.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner != nil && owner.UID == pool.UID {
			total++
			// Include terminating reservations in recovery; scale helpers already
			// exclude them when deciding which Pods to delete.
			pods = append(pods, pod)
		}
	}
	batchList := &sandboxv1alpha1.BatchSandboxList{}
	if err := r.APIReader.List(ctx, batchList, client.InNamespace(pool.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	var batches []*sandboxv1alpha1.BatchSandbox
	for i := range batchList.Items {
		if batchList.Items[i].Spec.PoolRef == pool.Name {
			batches = append(batches, &batchList.Items[i])
		}
	}
	result, err := r.scheduleUIDBoundPool(ctx, pool, batches, pods)
	if err != nil {
		return ctrl.Result{}, err
	}
	update, err := r.updatePool(ctx, pool, pods, result.IdlePods)
	if err != nil {
		return ctrl.Result{}, err
	}
	_, err = r.scalePool(ctx, pool, &scaleArgs{
		updateRevision: update.UpdateRevision,
		pods:           pods,
		totalPodCnt:    total,
		allocatedCnt:   int32(len(result.LatestAllocation)),
		supplyCnt:      result.SupplyCnt + update.SupplyUpdateRevision,
		idlePods:       update.IdlePods,
		toDeletePods:   update.ToDeletePods,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.updatePoolStatus(ctx, update.UpdateRevision, pool, pods, pods, result.LatestAllocation); err != nil {
		return ctrl.Result{}, err
	}
	// Reservations and deletion may be ahead of the informer cache.
	return ctrl.Result{RequeueAfter: defaultRetryTime}, nil
}

func (r *PoolReconciler) validateLiveBoundPool(ctx context.Context, pool *sandboxv1alpha1.Pool) error {
	if r.APIReader == nil {
		return fmt.Errorf("UID-bound allocation requires an uncached API reader")
	}
	live := &sandboxv1alpha1.Pool{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(pool), live); err != nil {
		return err
	}
	identity := &pkgutils.AllocationIdentity{Pool: pkgutils.ObjectIdentity{Name: pool.Name, UID: pool.UID}}
	if err := pkgutils.ValidateIdentityPool(identity, pool.Namespace, live); err != nil {
		return err
	}
	if !live.DeletionTimestamp.IsZero() {
		return fmt.Errorf("UID-bound Pool is terminating")
	}
	return nil
}

func pristineBoundBatch(bs *sandboxv1alpha1.BatchSandbox) error {
	for _, key := range []string{annoAllocStatusKey, annoAllocReleaseKey, annoAllocReleasedKey} {
		if _, exists := bs.Annotations[key]; exists {
			return fmt.Errorf("cannot upgrade an unbound BatchSandbox with allocation history")
		}
	}
	if raw, exists := bs.Annotations[pkgutils.AnnotationAllocationIntent]; exists {
		intent, err := pkgutils.ParseAllocationIdentity(raw)
		if err != nil || intent == nil || intent.BatchSandbox != (pkgutils.ObjectIdentity{Name: bs.Name, UID: bs.UID}) ||
			intent.Pool.Name != bs.Spec.PoolRef {
			return fmt.Errorf("invalid pending allocation intent")
		}
	} else if controllerutil.ContainsFinalizer(bs, finalizerPoolAllocation) {
		return fmt.Errorf("cannot upgrade an unbound BatchSandbox with allocation history")
	}
	if bs.Status.Allocated != 0 || bs.Status.Ready != 0 {
		return fmt.Errorf("cannot upgrade an unbound BatchSandbox with allocation history")
	}
	return nil
}

func newAllocationIdentity(pool *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) pkgutils.AllocationIdentity {
	return pkgutils.AllocationIdentity{
		Version:      pkgutils.AllocationModeUIDBoundV1,
		BatchSandbox: pkgutils.ObjectIdentity{Name: bs.Name, UID: bs.UID},
		Pool:         pkgutils.ObjectIdentity{Name: pool.Name, UID: pool.UID},
		Pod:          pkgutils.ObjectIdentity{Name: pod.Name, UID: pod.UID},
	}
}

// fencedAnnotationPatch does not mutate the input object on errors (including an
// ambiguous transport failure). Both UID and resourceVersion fence replacements.
func fencedAnnotationPatch(ctx context.Context, c client.Client, obj client.Object, annotations map[string]string, finalizers []string) error {
	if obj.GetUID() == "" || obj.GetResourceVersion() == "" {
		return fmt.Errorf("fenced publication requires UID and resourceVersion")
	}
	meta := map[string]any{
		"uid": obj.GetUID(), "resourceVersion": obj.GetResourceVersion(),
	}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	if finalizers != nil {
		meta["finalizers"] = finalizers
	}
	raw, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return err
	}
	target := obj.DeepCopyObject().(client.Object)
	return c.Patch(ctx, target, client.RawPatch(types.MergePatchType, raw))
}

// publishUIDBoundAllocation only accepts the exact objects selected by the
// scheduling round. Live reads validate those UIDs; they never supply new pins.
func (r *PoolReconciler) publishUIDBoundAllocation(ctx context.Context, pool *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) error {
	if _, err := validateAllocationModes(pool, []*sandboxv1alpha1.BatchSandbox{bs}); err != nil {
		return err
	}
	if err := r.validateLiveBoundPool(ctx, pool); err != nil {
		return err
	}
	identity := newAllocationIdentity(pool, bs, pod)
	if err := pkgutils.ValidateIdentityPod(&identity, bs.Namespace, pod); err != nil {
		return err
	}
	rawIdentity, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	liveBatch := &sandboxv1alpha1.BatchSandbox{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(bs), liveBatch); err != nil {
		return err
	}
	if liveBatch.UID != bs.UID || liveBatch.ResourceVersion != bs.ResourceVersion || !liveBatch.DeletionTimestamp.IsZero() {
		return fmt.Errorf("selected BatchSandbox changed before allocation publication")
	}
	if _, err := validateAllocationModes(pool, []*sandboxv1alpha1.BatchSandbox{liveBatch}); err != nil {
		return err
	}
	if _, exists := liveBatch.Annotations[pkgutils.AnnotationAllocationIdentity]; exists {
		existing, err := pkgutils.BatchAllocationIdentity(liveBatch)
		if err != nil {
			return err
		}
		if *existing != identity {
			return fmt.Errorf("cannot rebind an existing allocation identity")
		}
		_, err = pkgutils.ReadUIDBoundAllocation(ctx, r.APIReader, liveBatch)
		return err
	}
	if err := pristineBoundBatch(liveBatch); err != nil {
		return err
	}
	livePod := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(pod), livePod); err != nil {
		return err
	}
	if err := pkgutils.ValidateIdentityPod(&identity, bs.Namespace, livePod); err != nil {
		return err
	}
	if !livePod.DeletionTimestamp.IsZero() || !utils.IsPodReady(livePod) {
		return fmt.Errorf("selected Pod is no longer available")
	}
	if err := r.checkOtherReservations(ctx, bs, &identity); err != nil {
		return err
	}
	if raw, exists := liveBatch.Annotations[pkgutils.AnnotationAllocationIntent]; exists {
		intent, err := pkgutils.ParseAllocationIdentity(raw)
		if err != nil || intent == nil || *intent != identity {
			return fmt.Errorf("cannot change an existing allocation intent")
		}
	} else {
		finalizers := slices.Clone(liveBatch.Finalizers)
		if !slices.Contains(finalizers, finalizerPoolAllocation) {
			finalizers = append(finalizers, finalizerPoolAllocation)
		}
		if err := fencedAnnotationPatch(ctx, r.Client, liveBatch,
			map[string]string{pkgutils.AnnotationAllocationIntent: string(rawIdentity)}, finalizers); err != nil {
			return err
		}
		// Only the newly created intent permits advancing the Batch RV here.
		// Never advance a UID or reconstruct the originally selected identity.
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(bs), liveBatch); err != nil {
			return err
		}
		intent, err := pkgutils.ParseAllocationIdentity(liveBatch.Annotations[pkgutils.AnnotationAllocationIntent])
		if err != nil || intent == nil || *intent != identity || liveBatch.UID != bs.UID || !liveBatch.DeletionTimestamp.IsZero() {
			return fmt.Errorf("BatchSandbox changed after intent publication")
		}
		if _, err := validateAllocationModes(pool, []*sandboxv1alpha1.BatchSandbox{liveBatch}); err != nil {
			return err
		}
		if err := pristineBoundBatch(liveBatch); err != nil {
			return err
		}
	}
	if raw, reserved := livePod.Annotations[pkgutils.AnnotationAllocationIdentity]; reserved {
		existing, err := pkgutils.ParseAllocationIdentity(raw)
		if err != nil {
			return err
		}
		if *existing != identity {
			return fmt.Errorf("selected Pod is permanently reserved by a different allocation")
		}
	} else if err := fencedAnnotationPatch(ctx, r.Client, livePod,
		map[string]string{pkgutils.AnnotationAllocationIdentity: string(rawIdentity)}, nil); err != nil {
		return err
	}

	// A Pod reservation deliberately survives every later failure.
	if err := r.validateLiveBoundPool(ctx, pool); err != nil {
		return err
	}
	if err := r.checkOtherReservations(ctx, bs, &identity); err != nil {
		return err
	}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(pod), livePod); err != nil {
		return err
	}
	if err := pkgutils.ValidateIdentityPod(&identity, bs.Namespace, livePod); err != nil {
		return err
	}
	reservation, err := pkgutils.ParseAllocationIdentity(livePod.Annotations[pkgutils.AnnotationAllocationIdentity])
	if err != nil || reservation == nil || *reservation != identity || !livePod.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Pod reservation changed before publication")
	}
	status, err := json.Marshal(sandboxAllocation{Pods: []string{pod.Name}, PoolRef: pool.Name, Generation: liveBatch.Generation})
	if err != nil {
		return err
	}
	finalizers := slices.Clone(liveBatch.Finalizers)
	if !slices.Contains(finalizers, finalizerPoolAllocation) {
		finalizers = append(finalizers, finalizerPoolAllocation)
	}
	return fencedAnnotationPatch(ctx, r.Client, liveBatch, map[string]string{
		annoAllocStatusKey:                    string(status),
		pkgutils.AnnotationAllocationIdentity: string(rawIdentity),
	}, finalizers)
}

func (r *PoolReconciler) checkOtherReservations(ctx context.Context, bs *sandboxv1alpha1.BatchSandbox, identity *pkgutils.AllocationIdentity) error {
	batches := &sandboxv1alpha1.BatchSandboxList{}
	if err := r.APIReader.List(ctx, batches, client.InNamespace(bs.Namespace)); err != nil {
		return err
	}
	for i := range batches.Items {
		raw, exists := batches.Items[i].Annotations[pkgutils.AnnotationAllocationIntent]
		if !exists {
			continue
		}
		pin, err := pkgutils.ParseAllocationIdentity(raw)
		if err != nil {
			return err
		}
		if pin.Pod.UID == identity.Pod.UID && *pin != *identity {
			return fmt.Errorf("selected Pod UID already has another durable BatchSandbox intent")
		}
	}
	pods := &corev1.PodList{}
	if err := r.APIReader.List(ctx, pods, client.InNamespace(bs.Namespace)); err != nil {
		return err
	}
	for i := range pods.Items {
		raw, exists := pods.Items[i].Annotations[pkgutils.AnnotationAllocationIdentity]
		if !exists {
			continue
		}
		pin, err := pkgutils.ParseAllocationIdentity(raw)
		if err != nil {
			return err
		}
		if pin.BatchSandbox.UID == bs.UID && (*pin != *identity || pods.Items[i].UID != identity.Pod.UID) {
			return fmt.Errorf("BatchSandbox UID already has a different durable Pod reservation")
		}
	}
	return nil
}

func (r *PoolReconciler) scheduleUIDBoundPool(ctx context.Context, pool *sandboxv1alpha1.Pool, batches []*sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) (*scheduleResult, error) {
	if _, err := validateAllocationModes(pool, batches); err != nil {
		return nil, err
	}
	if err := r.validateLiveBoundPool(ctx, pool); err != nil {
		return nil, err
	}
	result := &scheduleResult{LatestAllocation: map[string]string{}}
	batchByUID := map[types.UID]*sandboxv1alpha1.BatchSandbox{}
	intentsByPod := map[string]*pkgutils.AllocationIdentity{}
	for _, bs := range batches {
		if batchByUID[bs.UID] != nil {
			return nil, fmt.Errorf("ambiguous BatchSandbox selection")
		}
		batchByUID[bs.UID] = bs
		if raw, exists := bs.Annotations[pkgutils.AnnotationAllocationIntent]; exists {
			intent, err := pkgutils.ParseAllocationIdentity(raw)
			if err != nil {
				return nil, err
			}
			if err := pkgutils.ValidateIdentityPool(intent, bs.Namespace, pool); err != nil {
				return nil, err
			}
			if existing := intentsByPod[intent.Pod.Name]; existing != nil && *existing != *intent {
				return nil, fmt.Errorf("ambiguous BatchSandbox intents for the same Pod")
			}
			intentsByPod[intent.Pod.Name] = intent
		}
		if _, exists := bs.Annotations[pkgutils.AnnotationAllocationIdentity]; !exists {
			if err := pristineBoundBatch(bs); err != nil {
				return nil, err
			}
		} else if _, err := pkgutils.BatchAllocationIdentity(bs); err != nil {
			return nil, err
		}
	}
	reserved := map[types.UID]*corev1.Pod{}
	var available []*corev1.Pod
	seen := map[string]bool{}
	for _, pod := range pods {
		if seen[pod.Name] {
			return nil, fmt.Errorf("ambiguous Pod selection")
		}
		seen[pod.Name] = true
		pin := newAllocationIdentity(pool, &sandboxv1alpha1.BatchSandbox{}, pod)
		if err := pkgutils.ValidateIdentityPod(&pin, pool.Namespace, pod); err != nil {
			return nil, err
		}
		intent := intentsByPod[pod.Name]
		if intent != nil && intent.Pod.UID != pod.UID {
			return nil, fmt.Errorf("Pod incarnation differs from durable allocation intent")
		}
		raw, claimed := pod.Annotations[pkgutils.AnnotationAllocationIdentity]
		if !claimed {
			if intent != nil {
				result.LatestAllocation[pod.Name] = intent.BatchSandbox.Name
				reserved[intent.BatchSandbox.UID] = pod
			} else if utils.IsPodReady(pod) && pod.DeletionTimestamp.IsZero() {
				available = append(available, pod)
			}
			continue
		}
		identity, err := pkgutils.ParseAllocationIdentity(raw)
		if err != nil {
			return nil, err
		}
		if intent != nil && *intent != *identity {
			return nil, fmt.Errorf("Pod reservation conflicts with durable BatchSandbox intent")
		}
		if err := pkgutils.ValidateIdentityPool(identity, pool.Namespace, pool); err != nil {
			return nil, err
		}
		if err := pkgutils.ValidateIdentityPod(identity, pool.Namespace, pod); err != nil {
			return nil, err
		}
		if reserved[identity.BatchSandbox.UID] != nil {
			return nil, fmt.Errorf("multiple Pod reservations for a single BatchSandbox UID")
		}
		reserved[identity.BatchSandbox.UID] = pod
		result.LatestAllocation[pod.Name] = identity.BatchSandbox.Name
		if bs := batchByUID[identity.BatchSandbox.UID]; bs != nil {
			if bs.Name != identity.BatchSandbox.Name {
				return nil, fmt.Errorf("reservation BatchSandbox name mismatch")
			}
			continue
		}
		// A stale list must not turn a live BatchSandbox into an orphan.
		live := &sandboxv1alpha1.BatchSandbox{}
		err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: identity.BatchSandbox.Name}, live)
		if err == nil && live.UID == identity.BatchSandbox.UID {
			return nil, fmt.Errorf("reserved BatchSandbox is missing from this scheduling snapshot")
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if err := r.deleteUIDBoundPod(ctx, pool, pod); err != nil {
			return nil, err
		}
	}
	for _, bs := range batches {
		if _, bound := bs.Annotations[pkgutils.AnnotationAllocationIdentity]; bound {
			identity, err := pkgutils.BatchAllocationIdentity(bs)
			if err != nil {
				return nil, err
			}
			if err := pkgutils.ValidateIdentityPool(identity, bs.Namespace, pool); err != nil {
				return nil, err
			}
			if pin := reserved[bs.UID]; pin != nil && pin.UID != identity.Pod.UID {
				return nil, fmt.Errorf("BatchSandbox binding conflicts with Pod reservation")
			}
			released, err := r.reconcileUIDBoundRelease(ctx, pool, bs, identity)
			if err != nil {
				return nil, err
			}
			if !released {
				result.LatestAllocation[identity.Pod.Name] = bs.Name
			}
			continue
		}
		if !bs.DeletionTimestamp.IsZero() {
			if raw, pending := bs.Annotations[pkgutils.AnnotationAllocationIntent]; pending {
				intent, err := pkgutils.ParseAllocationIdentity(raw)
				if err != nil {
					return nil, err
				}
				if err := r.reconcileUIDBoundIntentDeletion(ctx, pool, bs, intent); err != nil {
					return nil, err
				}
				continue
			}
			if pin := reserved[bs.UID]; pin != nil {
				if err := r.deleteUIDBoundPod(ctx, pool, pin); err != nil {
					return nil, err
				}
			}
			continue
		}
		pod := reserved[bs.UID]
		if raw, pending := bs.Annotations[pkgutils.AnnotationAllocationIntent]; pending {
			intent, err := pkgutils.ParseAllocationIdentity(raw)
			if err != nil {
				return nil, err
			}
			if err := pkgutils.ValidateIdentityPool(intent, bs.Namespace, pool); err != nil {
				return nil, err
			}
			pod = &corev1.Pod{}
			if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: bs.Namespace, Name: intent.Pod.Name}, pod); err != nil {
				return nil, err
			}
			if err := pkgutils.ValidateIdentityPod(intent, bs.Namespace, pod); err != nil {
				return nil, err
			}
			available = slices.DeleteFunc(available, func(candidate *corev1.Pod) bool { return candidate.UID == pod.UID })
		}
		if pod == nil {
			if len(available) == 0 {
				result.SupplyCnt++
				continue
			}
			pod, available = available[0], available[1:]
		}
		if err := r.publishUIDBoundAllocation(ctx, pool, bs, pod); err != nil {
			return nil, err
		}
		result.LatestAllocation[pod.Name] = bs.Name
	}
	for _, pod := range pods {
		if _, allocated := result.LatestAllocation[pod.Name]; !allocated {
			if _, claimed := pod.Annotations[pkgutils.AnnotationAllocationIdentity]; !claimed {
				result.IdlePods = append(result.IdlePods, pod.Name)
			}
		}
	}
	return result, nil
}

func (r *PoolReconciler) reconcileUIDBoundIntentDeletion(ctx context.Context, pool *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, intent *pkgutils.AllocationIdentity) error {
	if err := pkgutils.ValidateIdentityPool(intent, bs.Namespace, pool); err != nil {
		return err
	}
	if err := r.validateLiveBoundPool(ctx, pool); err != nil {
		return err
	}
	pod := &corev1.Pod{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: bs.Namespace, Name: intent.Pod.Name}, pod)
	if err == nil {
		if err := pkgutils.ValidateIdentityPod(intent, bs.Namespace, pod); err != nil {
			return err
		}
		if raw, claimed := pod.Annotations[pkgutils.AnnotationAllocationIdentity]; claimed {
			reservation, err := pkgutils.ParseAllocationIdentity(raw)
			if err != nil || reservation == nil || *reservation != *intent {
				return fmt.Errorf("cannot recycle another allocation's reserved Pod")
			}
		}
		return r.deleteUIDBoundPod(ctx, pool, pod)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	finalizers := slices.DeleteFunc(slices.Clone(bs.Finalizers), func(f string) bool { return f == finalizerPoolAllocation || f == finalizerTaskCleanup })
	if finalizers == nil {
		finalizers = []string{}
	}
	return fencedAnnotationPatch(ctx, r.Client, bs, nil, finalizers)
}

func (r *PoolReconciler) deleteUIDBoundPod(ctx context.Context, pool *sandboxv1alpha1.Pool, pod *corev1.Pod) error {
	if err := r.validateLiveBoundPool(ctx, pool); err != nil {
		return err
	}
	if pod.UID == "" || pod.ResourceVersion == "" {
		return fmt.Errorf("cannot delete an unfenced Pod")
	}
	return client.IgnoreNotFound(r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}))
}

func (r *PoolReconciler) reconcileUIDBoundRelease(ctx context.Context, pool *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, identity *pkgutils.AllocationIdentity) (bool, error) {
	live := &sandboxv1alpha1.BatchSandbox{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(bs), live); err != nil {
		return false, err
	}
	current, err := pkgutils.BatchAllocationIdentity(live)
	if err != nil {
		return false, err
	}
	if *current != *identity {
		return false, fmt.Errorf("BatchSandbox identity changed before recycle")
	}
	releasing := !live.DeletionTimestamp.IsZero()
	for _, key := range []string{annoAllocReleaseKey, annoAllocReleasedKey} {
		names, valid := validLegacyAllocationRelease(live.Annotations, key)
		if !valid || len(names) > 1 || (len(names) == 1 && names[0] != identity.Pod.Name) {
			return false, fmt.Errorf("invalid UID-bound release state")
		}
		releasing = releasing || len(names) == 1
	}
	if !releasing {
		_, err := pkgutils.ReadUIDBoundAllocation(ctx, r.APIReader, bs)
		return false, err
	}
	if err := r.validateLiveBoundPool(ctx, pool); err != nil {
		return false, err
	}
	pod := &corev1.Pod{}
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: bs.Namespace, Name: identity.Pod.Name}, pod)
	if err == nil {
		if err := pkgutils.ValidateIdentityPod(identity, bs.Namespace, pod); err != nil {
			return false, err
		}
		reservation, err := pkgutils.ParseAllocationIdentity(pod.Annotations[pkgutils.AnnotationAllocationIdentity])
		if err != nil || reservation == nil || *reservation != *identity {
			return false, fmt.Errorf("Pod reservation changed before recycle")
		}
		if pod.DeletionTimestamp.IsZero() {
			return false, r.deleteUIDBoundPod(ctx, pool, pod)
		}
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	raw, err := json.Marshal(allocationReleased{Pods: []string{identity.Pod.Name}})
	if err != nil {
		return false, err
	}
	finalizers := slices.Clone(live.Finalizers)
	if !live.DeletionTimestamp.IsZero() {
		finalizers = slices.DeleteFunc(finalizers, func(f string) bool { return f == finalizerPoolAllocation || f == finalizerTaskCleanup })
		if finalizers == nil {
			finalizers = []string{}
		}
	}
	if live.Annotations[annoAllocReleasedKey] == string(raw) && slices.Equal(live.Finalizers, finalizers) {
		return true, nil
	}
	err = fencedAnnotationPatch(ctx, r.Client, live, map[string]string{annoAllocReleasedKey: string(raw)}, finalizers)
	return err == nil, err
}
