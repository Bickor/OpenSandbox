// Copyright 2026 Alibaba Group Holding Ltd.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	pkgutils "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/utils"
)

func TestUIDBoundEnvtest(t *testing.T) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	prototype, _, _, _ := boundFixture(t)
	c, err := client.New(cfg, client.Options{Scheme: prototype.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resources := func(t *testing.T) (*PoolReconciler, *sandboxv1alpha1.Pool, *sandboxv1alpha1.BatchSandbox, *corev1.Pod) {
		t.Helper()
		r, pool, bs, pods := boundFixture(t)
		r.Client, r.APIReader, r.Recorder = c, c, record.NewFakeRecorder(100)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "uid-bound-"}}
		if err := c.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
		for _, obj := range []client.Object{pool, bs, pods[0]} {
			obj.SetNamespace(ns.Name)
			obj.SetUID("")
			obj.SetResourceVersion("")
		}
		if err := c.Create(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if err := c.Create(ctx, bs); err != nil {
			t.Fatal(err)
		}
		pod := pods[0]
		status := pod.Status
		pod.Status = corev1.PodStatus{}
		pod.Spec = pool.Spec.Template.Spec
		pod.Spec.NodeName = "envtest-node"
		pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(pool, sandboxv1alpha1.GroupVersion.WithKind("Pool"))}
		if err := c.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status = status
		if err := c.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
		return r, pool, bs, pod
	}

	t.Run("atomic publication delete and replenish", func(t *testing.T) {
		r, pool, bs, pod := resources(t)
		atomic := false
		r.Client = &boundPatchClient{Client: c, beforePatch: func(_ context.Context, obj client.Object, patch client.Patch) error {
			if isFinalBoundPublication(obj, patch) {
				raw, _ := patch.Data(obj)
				var data struct {
					Metadata struct {
						UID             string            `json:"uid"`
						ResourceVersion string            `json:"resourceVersion"`
						Annotations     map[string]string `json:"annotations"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(raw, &data); err != nil {
					return err
				}
				atomic = data.Metadata.UID == string(bs.UID) && data.Metadata.ResourceVersion != "" &&
					data.Metadata.Annotations[pkgutils.AnnotationAllocationIdentity] != "" &&
					data.Metadata.Annotations[annoAllocStatusKey] != ""
			}
			return nil
		}}
		if err := r.publishUIDBoundAllocation(ctx, pool, bs, pod); err != nil {
			t.Fatal(err)
		}
		if !atomic {
			t.Fatal("allocation and identity were not atomically UID/RV fenced")
		}
		r.Client = c
		refreshBound(t, c, bs, []*corev1.Pod{pod})
		if _, err := pkgutils.ReadUIDBoundAllocation(ctx, c, bs); err != nil {
			t.Fatal(err)
		}
		if _, err := r.reconcileUIDBoundPool(ctx, pool); err != nil {
			t.Fatal(err)
		}
		bs.Finalizers = append(bs.Finalizers, finalizerTaskCleanup)
		if err := c.Update(ctx, bs); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(ctx, bs); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := c.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
				t.Fatal(err)
			}
			if _, err := r.reconcileUIDBoundPool(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				deletingPod := &corev1.Pod{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(pod), deletingPod); err != nil {
					t.Fatal(err)
				}
				if deletingPod.DeletionTimestamp.IsZero() {
					t.Fatal("recycle did not request Pod deletion")
				}
				terminatingBatch := &sandboxv1alpha1.BatchSandbox{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(bs), terminatingBatch); err != nil {
					t.Fatal("Batch was finalized before the Pod disappeared")
				}
				// envtest has no kubelet to complete graceful Pod deletion.
				if err := c.Delete(ctx, deletingPod, client.GracePeriodSeconds(0), client.Preconditions{UID: &deletingPod.UID}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
			t.Fatalf("used Pod was retained: %v", err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(bs), &sandboxv1alpha1.BatchSandbox{}); !apierrors.IsNotFound(err) {
			t.Fatalf("BatchSandbox cleanup did not complete after Pod deletion: %v", err)
		}
		podList := &corev1.PodList{}
		if err := c.List(ctx, podList, client.InNamespace(pool.Namespace)); err != nil {
			t.Fatal(err)
		}
		if len(podList.Items) == 0 {
			t.Fatal("Pool buffer was not replenished")
		}
		for _, replacement := range podList.Items {
			if replacement.UID == pod.UID || replacement.Annotations[pkgutils.AnnotationAllocationMode] != pkgutils.AllocationModeUIDBoundV1 {
				t.Fatal("replenishment reused the old Pod or lost its protected marker")
			}
		}
	})

	for _, replace := range []bool{false, true} {
		name := "resourceVersion conflict"
		if replace {
			name = "late replacement BatchSandbox"
		}
		t.Run(name, func(t *testing.T) {
			r, pool, bs, pod := resources(t)
			r.Client = &boundPatchClient{Client: c, beforePatch: func(ctx context.Context, obj client.Object, patch client.Patch) error {
				if !isFinalBoundPublication(obj, patch) {
					return nil
				}
				current := &sandboxv1alpha1.BatchSandbox{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(bs), current); err != nil {
					return err
				}
				if !replace {
					current.Labels = map[string]string{"concurrent": "writer"}
					return c.Update(ctx, current)
				}
				current.Finalizers = nil
				if err := c.Update(ctx, current); err != nil {
					return err
				}
				if err := c.Delete(ctx, current); err != nil {
					return err
				}
				replacement := bs.DeepCopy()
				replacement.UID, replacement.ResourceVersion = "", ""
				return c.Create(ctx, replacement)
			}}
			if err := r.publishUIDBoundAllocation(ctx, pool, bs, pod); err == nil {
				t.Fatal("stale allocation publication unexpectedly succeeded")
			}
			current := &sandboxv1alpha1.BatchSandbox{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(bs), current); err != nil {
				t.Fatal(err)
			}
			if current.Annotations[annoAllocStatusKey] != "" || current.Annotations[pkgutils.AnnotationAllocationIdentity] != "" {
				t.Fatal("failed publication modified final allocation evidence")
			}
			if !replace && current.Annotations[pkgutils.AnnotationAllocationIntent] == "" {
				t.Fatal("conflict lost original intent")
			}
		})
	}
}
