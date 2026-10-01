// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func identityFixture(t *testing.T) (*sandboxv1alpha1.Pool, *sandboxv1alpha1.BatchSandbox, *corev1.Pod, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "test", Annotations: map[string]string{AnnotationAllocationMode: AllocationModeUIDBoundV1}}
	}
	pool := &sandboxv1alpha1.Pool{ObjectMeta: meta("pool")}
	pool.UID = "pool-uid"
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: meta("batch"), Spec: sandboxv1alpha1.BatchSandboxSpec{PoolRef: pool.Name, Replicas: ptr.To(int32(1))}}
	bs.UID = "batch-uid"
	pod := &corev1.Pod{ObjectMeta: meta("pod"), Status: corev1.PodStatus{
		Phase: corev1.PodRunning, PodIP: "10.0.0.1",
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}}
	pod.UID = "pod-uid"
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pool, sandboxv1alpha1.GroupVersion.WithKind("Pool"))}
	identity := AllocationIdentity{
		Version: AllocationModeUIDBoundV1, BatchSandbox: ObjectIdentity{Name: bs.Name, UID: bs.UID},
		Pool: ObjectIdentity{Name: pool.Name, UID: pool.UID}, Pod: ObjectIdentity{Name: pod.Name, UID: pod.UID},
	}
	raw, _ := json.Marshal(identity)
	bs.Annotations[AnnotationAllocationIdentity] = string(raw)
	bs.Annotations[AnnotationAllocationIntent] = string(raw)
	pod.Annotations[AnnotationAllocationIdentity] = string(raw)
	bs.Annotations[AnnotationAllocationStatus] = `{"pods":["pod"],"poolRef":"pool","generation":1}`
	bs.Annotations[AnnotationEndpoints] = `["10.0.0.1"]`
	return pool, bs, pod, scheme
}

func TestUIDBoundReaders(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sandboxv1alpha1.Pool, *sandboxv1alpha1.BatchSandbox, *corev1.Pod)
		valid  bool
	}{
		{name: "valid", valid: true},
		{name: "default replicas", valid: true, mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) { bs.Spec.Replicas = nil }},
		{name: "generation is not freshness", valid: true, mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) { bs.Generation = 42 }},
		{name: "missing binding", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			delete(bs.Annotations, AnnotationAllocationIdentity)
		}},
		{name: "missing intent", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			delete(bs.Annotations, AnnotationAllocationIntent)
		}},
		{name: "binding without marker", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			delete(bs.Annotations, AnnotationAllocationMode)
		}},
		{name: "unknown mode", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations[AnnotationAllocationMode] = "uid-bound-v2"
		}},
		{name: "corrupt binding", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations[AnnotationAllocationIdentity] = "{"
		}},
		{name: "unsupported version", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations[AnnotationAllocationIdentity] = strings.ReplaceAll(bs.Annotations[AnnotationAllocationIdentity], AllocationModeUIDBoundV1, "v2")
		}},
		{name: "changed intent", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations[AnnotationAllocationIntent] = `{}`
		}},
		{name: "replaced batch", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) { bs.UID = "replacement" }},
		{name: "replaced pool", mutate: func(pool *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			pool.UID = "replacement"
		}},
		{name: "replaced pod", mutate: func(_ *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) {
			pod.UID = "replacement"
		}},
		{name: "wrong pod owner", mutate: func(_ *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) {
			pod.OwnerReferences[0].UID = "other"
		}},
		{name: "missing reservation", mutate: func(_ *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) {
			delete(pod.Annotations, AnnotationAllocationIdentity)
		}},
		{name: "wrong reservation", mutate: func(_ *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, pod *corev1.Pod) {
			pod.Annotations[AnnotationAllocationIdentity] = strings.ReplaceAll(pod.Annotations[AnnotationAllocationIdentity], "batch-uid", "other")
		}},
		{name: "missing pool marker", mutate: func(pool *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			delete(pool.Annotations, AnnotationAllocationMode)
		}},
		{name: "pods-only protected status", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations[AnnotationAllocationStatus] = `{"pods":["pod"]}`
		}},
		{name: "wrong status pod", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations[AnnotationAllocationStatus] = `{"pods":["other"],"poolRef":"pool"}`
		}},
		{name: "unsupported replicas", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Spec.Replicas = ptr.To(int32(2))
		}},
		{name: "unsupported recycle", mutate: func(pool *sandboxv1alpha1.Pool, _ *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			pool.Spec.RecycleStrategy = &sandboxv1alpha1.RecycleStrategy{Type: sandboxv1alpha1.RecycleTypeNoop}
		}},
		{name: "released allocation", mutate: func(_ *sandboxv1alpha1.Pool, bs *sandboxv1alpha1.BatchSandbox, _ *corev1.Pod) {
			bs.Annotations["sandbox.opensandbox.io/alloc-released"] = `{"pods":["pod"]}`
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pool, bs, pod, scheme := identityFixture(t)
			if test.mutate != nil {
				test.mutate(pool, bs, pod)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, bs, pod).Build()
			_, err := ReadUIDBoundAllocation(context.Background(), c, bs)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
			_, err = GetEndpointsWithReader(context.Background(), c, bs)
			if (err == nil) != test.valid {
				t.Fatalf("endpoint valid=%v, error=%v", test.valid, err)
			}
			if _, err := GetEndpoints(bs); err == nil {
				t.Fatal("pure endpoint reader must never accept protected identity")
			}
		})
	}
}

func TestUIDBoundLiveReadersFenceStaleBatch(t *testing.T) {
	pool, bs, pod, scheme := identityFixture(t)
	stale := bs.DeepCopy()
	bs.UID = "replacement"
	bs.Annotations[AnnotationAllocationIdentity] = strings.ReplaceAll(bs.Annotations[AnnotationAllocationIdentity], "batch-uid", "replacement")
	bs.Annotations[AnnotationAllocationIntent] = bs.Annotations[AnnotationAllocationIdentity]
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, bs, pod).Build()
	if _, err := ReadUIDBoundAllocation(context.Background(), c, stale); err == nil {
		t.Fatal("accepted replaced live BatchSandbox")
	}
}

func TestUIDBoundRejectAmbiguousJSON(t *testing.T) {
	_, bs, _, _ := identityFixture(t)
	raw := bs.Annotations[AnnotationAllocationIdentity]
	for _, invalid := range []string{"", "null", "{}", raw + raw,
		strings.Replace(raw, `"version":`, `"version":"wrong","version":`, 1),
		strings.Replace(raw, `"uid":"pod-uid"`, `"uid":"pod-uid","uid":"pod-uid"`, 1),
		strings.Replace(raw, `"version":`, `"unknown":1,"version":`, 1),
	} {
		if _, err := ParseAllocationIdentity(invalid); err == nil {
			t.Fatalf("accepted ambiguous identity: %s", invalid)
		}
	}
}

func TestUIDBoundNativeEndpointsRemainReadFree(t *testing.T) {
	bs := &sandboxv1alpha1.BatchSandbox{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationEndpoints: `["10.0.0.1","10.0.0.2"]`, AnnotationAllocationStatus: `{"pods":["a","b"]}`,
	}}}
	endpoints, err := GetEndpointsWithReader(context.Background(), nil, bs)
	if err != nil || len(endpoints) != 2 {
		t.Fatalf("legacy endpoints changed: %v %v", endpoints, err)
	}
}
