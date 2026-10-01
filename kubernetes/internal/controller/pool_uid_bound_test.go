// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/controller/strategy"
	pkgutils "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/utils"
)

func boundFixture(t *testing.T) (*PoolReconciler, *sandboxv1alpha1.Pool, *sandboxv1alpha1.BatchSandbox, []*corev1.Pod) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "test", UID: types.UID(name + "-uid"), ResourceVersion: "1",
			Annotations: map[string]string{pkgutils.AnnotationAllocationMode: pkgutils.AllocationModeUIDBoundV1}}
	}
	pool := &sandboxv1alpha1.Pool{ObjectMeta: meta("pool"), Spec: sandboxv1alpha1.PoolSpec{
		Template:     &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandbox", Image: "pause"}}}},
		CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMin: 1, PoolMax: 3, BufferMin: 1, BufferMax: 1},
	}}
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: meta("batch"),
		Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(1))}}
	pods := []*corev1.Pod{}
	for i := 0; i < 2; i++ {
		pod := &corev1.Pod{ObjectMeta: meta(fmt.Sprintf("pod-%d", i)), Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: fmt.Sprintf("10.0.0.%d", i+1),
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		}}
		pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pool, sandboxv1alpha1.GroupVersion.WithKind("Pool"))}
		pods = append(pods, pod)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}, &sandboxv1alpha1.BatchSandbox{}, &sandboxv1alpha1.Pool{}).
		WithObjects(pool, bs, pods[0], pods[1]).Build()
	return &PoolReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100)}, pool, bs, pods
}

func refreshBound(t *testing.T, c client.Client, bs *sandboxv1alpha1.BatchSandbox, pods []*corev1.Pod) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(bs), bs); err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), pod); err != nil {
			t.Fatal(err)
		}
	}
}

type boundPatchClient struct {
	client.Client
	beforePatch func(context.Context, client.Object, client.Patch) error
}

func (c *boundPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.beforePatch(ctx, obj, patch); err != nil {
		return err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func isFinalBoundPublication(obj client.Object, patch client.Patch) bool {
	if _, ok := obj.(*sandboxv1alpha1.BatchSandbox); !ok {
		return false
	}
	data, _ := patch.Data(obj)
	var body struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(data, &body)
	_, exists := body.Metadata.Annotations[annoAllocStatusKey]
	return exists
}

func TestUIDBoundPublication(t *testing.T) {
	r, pool, bs, pods := boundFixture(t)
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err != nil {
		t.Fatal(err)
	}
	refreshBound(t, r.Client, bs, pods)
	identity, err := pkgutils.BatchAllocationIdentity(bs)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Pod.UID != pods[0].UID || identity.BatchSandbox.UID != bs.UID || identity.Pool.UID != pool.UID {
		t.Fatalf("incorrect identity: %+v", identity)
	}
	if bs.Annotations[pkgutils.AnnotationAllocationIntent] != bs.Annotations[pkgutils.AnnotationAllocationIdentity] ||
		pods[0].Annotations[pkgutils.AnnotationAllocationIdentity] != bs.Annotations[pkgutils.AnnotationAllocationIdentity] {
		t.Fatal("durable pins do not match")
	}
	if _, err := pkgutils.ReadUIDBoundAllocation(context.Background(), r.APIReader, bs); err != nil {
		t.Fatal(err)
	}
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err != nil {
		t.Fatalf("idempotent publication failed: %v", err)
	}
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[1]); err == nil {
		t.Fatal("existing binding silently rebound")
	}
}

func TestUIDBoundPublicationFencesSelectedObjects(t *testing.T) {
	for _, object := range []string{"pool", "batch", "pod", "stale-batch"} {
		t.Run(object, func(t *testing.T) {
			r, pool, bs, pods := boundFixture(t)
			var updated client.Object
			switch object {
			case "pool":
				updated = pool.DeepCopy()
			case "batch", "stale-batch":
				updated = bs.DeepCopy()
			default:
				updated = pods[0].DeepCopy()
			}
			if object == "stale-batch" {
				updated.SetLabels(map[string]string{"other-writer": "yes"})
			} else {
				updated.SetUID("replacement")
			}
			if err := r.Update(context.Background(), updated); err != nil {
				t.Fatal(err)
			}
			if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err == nil {
				t.Fatal("published against replaced/stale selection")
			}
			live := &sandboxv1alpha1.BatchSandbox{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(bs), live); err != nil {
				t.Fatal(err)
			}
			if _, exists := live.Annotations[annoAllocStatusKey]; exists {
				t.Fatal("allocation status published despite rejected identity")
			}
		})
	}
}

func TestUIDBoundRejectUnsupportedOrUnbound(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sandboxv1alpha1.Pool, *sandboxv1alpha1.BatchSandbox, *corev1.Pod)
	}{
		{"replicas", func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Spec.Replicas = ptr.To(int32(2))
		}},
		{"zero replicas", func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Spec.Replicas = ptr.To(int32(0))
		}},
		{"restart", func(pool *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			pool.Spec.RecycleStrategy = &sandboxv1alpha1.RecycleStrategy{Type: sandboxv1alpha1.RecycleTypeRestart}
		}},
		{"noop", func(pool *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			pool.Spec.RecycleStrategy = &sandboxv1alpha1.RecycleStrategy{Type: sandboxv1alpha1.RecycleTypeNoop}
		}},
		{"pool marker only", func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			delete(bs.Annotations, pkgutils.AnnotationAllocationMode)
		}},
		{"batch marker only", func(pool *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			delete(pool.Annotations, pkgutils.AnnotationAllocationMode)
		}},
		{"old unmarked pod", func(_ *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) {
			delete(pod.Annotations, pkgutils.AnnotationAllocationMode)
		}},
		{"legacy allocation", func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations[annoAllocStatusKey] = `{"pods":["pod-0"]}`
		}},
		{"empty allocation history", func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations[annoAllocStatusKey] = ""
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, pool, bs, pods := boundFixture(t)
			test.mutate(pool, bs, pods[0])
			if _, err := r.scheduleUIDBoundPool(context.Background(), pool, []*sandboxv1alpha1.BatchSandbox{bs}, pods); err == nil {
				t.Fatal("unsupported allocation accepted")
			}
		})
	}
}

func TestUIDBoundRecoveryRetainsPins(t *testing.T) {
	for _, lostPod := range []bool{false, true} {
		t.Run(fmt.Sprintf("lostPod=%v", lostPod), func(t *testing.T) {
			r, pool, bs, pods := boundFixture(t)
			realClient := r.Client
			r.Client = &boundPatchClient{Client: realClient, beforePatch: func(_ context.Context, obj client.Object, patch client.Patch) error {
				if isFinalBoundPublication(obj, patch) {
					return errors.New("simulated final publication conflict")
				}
				return nil
			}}
			if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err == nil {
				t.Fatal("expected publication failure")
			}
			refreshBound(t, realClient, bs, pods)
			if bs.Annotations[pkgutils.AnnotationAllocationIntent] == "" || pods[0].Annotations[pkgutils.AnnotationAllocationIdentity] == "" {
				t.Fatal("failed publication lost durable pins")
			}
			if _, exists := bs.Annotations[annoAllocStatusKey]; exists {
				t.Fatal("alloc-status published without successful atomic operation")
			}
			// A completely new reconciler recovers without any in-memory state.
			restarted := &PoolReconciler{Client: realClient, APIReader: realClient}
			if lostPod {
				if err := realClient.Delete(context.Background(), pods[0]); err != nil {
					t.Fatal(err)
				}
				replacement := pods[0].DeepCopy()
				replacement.ResourceVersion = ""
				replacement.UID = "replacement-pod"
				delete(replacement.Annotations, pkgutils.AnnotationAllocationIdentity)
				if err := realClient.Create(context.Background(), replacement); err != nil {
					t.Fatal(err)
				}
				pods[0] = replacement
			}
			_, err := restarted.scheduleUIDBoundPool(context.Background(), pool, []*sandboxv1alpha1.BatchSandbox{bs}, []*corev1.Pod{pods[1], pods[0]})
			if lostPod {
				if err == nil {
					t.Fatal("rebound the lost Pod intent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			refreshBound(t, realClient, bs, pods)
			identity, err := pkgutils.BatchAllocationIdentity(bs)
			if err != nil || identity.Pod.UID != pods[0].UID {
				t.Fatalf("recovery selected a different Pod: %+v %v", identity, err)
			}
		})
	}
}

func TestUIDBoundConflictAfterIntentPreservesPins(t *testing.T) {
	r, pool, bs, pods := boundFixture(t)
	base := r.Client
	r.Client = &boundPatchClient{Client: base, beforePatch: func(ctx context.Context, obj client.Object, patch client.Patch) error {
		if isFinalBoundPublication(obj, patch) {
			live := &sandboxv1alpha1.BatchSandbox{}
			if err := base.Get(ctx, client.ObjectKeyFromObject(bs), live); err != nil {
				return err
			}
			live.Labels = map[string]string{"concurrent": "update"}
			return base.Update(ctx, live)
		}
		return nil
	}}
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); !apierrors.IsConflict(err) {
		t.Fatalf("wanted optimistic conflict, got %v", err)
	}
	refreshBound(t, base, bs, pods)
	if bs.Annotations[annoAllocStatusKey] != "" || bs.Annotations[pkgutils.AnnotationAllocationIntent] == "" {
		t.Fatal("conflict did not preserve unpublished intent")
	}
}

func TestUIDBoundRecoveryBeforePodReservation(t *testing.T) {
	r, pool, bs, pods := boundFixture(t)
	base := r.Client
	r.Client = &boundPatchClient{Client: base, beforePatch: func(_ context.Context, obj client.Object, _ client.Patch) error {
		if _, ok := obj.(*corev1.Pod); ok {
			return errors.New("interrupted before Pod reservation")
		}
		return nil
	}}
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err == nil {
		t.Fatal("expected reservation failure")
	}
	refreshBound(t, base, bs, pods)
	if bs.Annotations[pkgutils.AnnotationAllocationIntent] == "" || pods[0].Annotations[pkgutils.AnnotationAllocationIdentity] != "" {
		t.Fatal("did not stop between intent and reservation")
	}
	next := bs.DeepCopy()
	next.Name, next.UID, next.ResourceVersion, next.Finalizers = "next", "next-uid", "", nil
	next.Annotations = map[string]string{pkgutils.AnnotationAllocationMode: pkgutils.AllocationModeUIDBoundV1}
	if err := base.Create(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	restarted := &PoolReconciler{Client: base, APIReader: base}
	// The new batch is deliberately processed first. It must not steal the Pod
	// pinned by the interrupted intent, even though that Pod is still unclaimed.
	if _, err := restarted.scheduleUIDBoundPool(context.Background(), pool, []*sandboxv1alpha1.BatchSandbox{next, bs}, pods); err != nil {
		t.Fatal(err)
	}
	refreshBound(t, base, bs, pods)
	refreshBound(t, base, next, nil)
	original, err := pkgutils.BatchAllocationIdentity(bs)
	if err != nil {
		t.Fatal(err)
	}
	other, err := pkgutils.BatchAllocationIdentity(next)
	if err != nil {
		t.Fatal(err)
	}
	if original.Pod.UID != pods[0].UID || other.Pod.UID != pods[1].UID {
		t.Fatalf("pending intent was stolen: original=%+v next=%+v", original, other)
	}
}

func TestUIDBoundOrphanReservationDeletesRatherThanRebinds(t *testing.T) {
	r, pool, bs, pods := boundFixture(t)
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err != nil {
		t.Fatal(err)
	}
	refreshBound(t, r.Client, bs, pods)
	bs.Finalizers = nil
	if err := r.Update(context.Background(), bs); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(context.Background(), bs); err != nil {
		t.Fatal(err)
	}
	replacement := bs.DeepCopy()
	replacement.UID, replacement.ResourceVersion = "replacement-batch", ""
	replacement.Annotations = map[string]string{pkgutils.AnnotationAllocationMode: pkgutils.AllocationModeUIDBoundV1}
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := r.scheduleUIDBoundPool(context.Background(), pool, []*sandboxv1alpha1.BatchSandbox{replacement}, pods); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pods[0]), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan reservation was not deleted: %v", err)
	}
	refreshBound(t, r.Client, replacement, nil)
	identity, err := pkgutils.BatchAllocationIdentity(replacement)
	if err != nil || identity.Pod.UID == pods[0].UID {
		t.Fatalf("replacement batch reused the orphan Pod: %+v %v", identity, err)
	}
}

func TestUIDBoundNativeRecoveryRemainsSeparate(t *testing.T) {
	r, pool, bs, pods := boundFixture(t)
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err != nil {
		t.Fatal(err)
	}
	native := &sandboxv1alpha1.BatchSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "native", Namespace: pool.Namespace, Annotations: map[string]string{annoAllocStatusKey: `{"pods":["native-pod"]}`}},
		Spec:       sandboxv1alpha1.BatchSandboxSpec{PoolRef: "native-pool"},
	}
	if err := r.Create(context.Background(), native); err != nil {
		t.Fatal(err)
	}
	store := newInMemoryAllocationStore()
	if err := store.Recover(context.Background(), r.Client); err != nil {
		t.Fatal(err)
	}
	protected, err := store.GetAllocation(context.Background(), pool)
	if err != nil || len(protected.PodAllocation) != 0 {
		t.Fatalf("protected identity leaked into legacy store: %+v %v", protected, err)
	}
	legacy, err := store.GetAllocation(context.Background(), &sandboxv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "native-pool", Namespace: pool.Namespace}})
	if err != nil || legacy.PodAllocation["native-pod"] != native.Name {
		t.Fatalf("legacy recovery changed: %+v %v", legacy, err)
	}
}

func TestUIDBoundDeleteNeverReusesPod(t *testing.T) {
	r, pool, bs, pods := boundFixture(t)
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err != nil {
		t.Fatal(err)
	}
	refreshBound(t, r.Client, bs, pods)
	bs.Annotations[annoAllocReleaseKey] = `{"pods":["pod-0"]}`
	if err := r.Update(context.Background(), bs); err != nil {
		t.Fatal(err)
	}
	if _, err := r.scheduleUIDBoundPool(context.Background(), pool, []*sandboxv1alpha1.BatchSandbox{bs}, pods); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pods[0]), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("used Pod was not deleted: %v", err)
	}
	refreshBound(t, r.Client, bs, pods[1:])
	next := bs.DeepCopy()
	next.Name, next.UID, next.ResourceVersion = "next", "next-uid", ""
	next.Annotations = map[string]string{pkgutils.AnnotationAllocationMode: pkgutils.AllocationModeUIDBoundV1}
	next.Finalizers = nil
	if err := r.Create(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if _, err := r.scheduleUIDBoundPool(context.Background(), pool, []*sandboxv1alpha1.BatchSandbox{bs, next}, pods[1:]); err != nil {
		t.Fatal(err)
	}
	refreshBound(t, r.Client, next, nil)
	binding, err := pkgutils.BatchAllocationIdentity(next)
	if err != nil || binding.Pod.UID == pods[0].UID || binding.Pod.UID != pods[1].UID {
		t.Fatalf("used Pod was reused: %+v %v", binding, err)
	}
}

func TestUIDBoundControllerReadersBlockMissingOrReplacedIdentity(t *testing.T) {
	r, pool, bs, pods := boundFixture(t)
	batch := &BatchSandboxReconciler{Client: r.Client, APIReader: r.APIReader}
	if _, err := batch.listPods(context.Background(), strategy.NewPoolStrategy(bs), bs); err == nil {
		t.Fatal("reader accepted missing binding")
	}
	if err := r.publishUIDBoundAllocation(context.Background(), pool, bs, pods[0]); err != nil {
		t.Fatal(err)
	}
	refreshBound(t, r.Client, bs, pods)
	if _, err := batch.listPods(context.Background(), strategy.NewPoolStrategy(bs), bs); err != nil {
		t.Fatal(err)
	}
	replaced := pods[0].DeepCopy()
	replaced.UID = "replacement"
	if err := r.Update(context.Background(), replaced); err != nil {
		t.Fatal(err)
	}
	if _, err := batch.listPods(context.Background(), strategy.NewPoolStrategy(bs), bs); err == nil {
		t.Fatal("reader accepted replaced Pod")
	}
	if err := batch.patchBatchSandboxEndpoints(context.Background(), bs, []string{pods[0].Status.PodIP}); err == nil {
		t.Fatal("endpoint publisher accepted replaced Pod")
	}
	snapshot := &SandboxSnapshotReconciler{Client: r.Client, APIReader: r.APIReader}
	if _, err := snapshot.findPodForSandbox(context.Background(), bs, bs.Namespace); err == nil {
		t.Fatal("snapshot reader fell back after failed identity")
	}
}

func TestUIDBoundNativePodsOnlyUnchanged(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{annoAllocStatusKey: `{"pods":["a","b"]}`}}}
	alloc, err := parseSandboxAllocation(bs)
	if err != nil || len(alloc.Pods) != 2 || alloc.PoolRef != "" || alloc.Generation != 0 {
		t.Fatalf("legacy parsing changed: %+v %v", alloc, err)
	}
	// A nil client proves the legacy read performs no additional API calls.
	allocPtr, err := (&annoAllocationSyncer{}).GetAllocation(context.Background(), bs)
	if err != nil || len(allocPtr.Pods) != 2 {
		t.Fatalf("legacy allocator read changed: %+v %v", allocPtr, err)
	}
	if mode, err := validateAllocationModes(&sandboxv1alpha1.Pool{}, []*sandboxv1alpha1.BatchSandbox{bs}); mode || err != nil {
		t.Fatalf("native mode changed: %v %v", mode, err)
	}
}
