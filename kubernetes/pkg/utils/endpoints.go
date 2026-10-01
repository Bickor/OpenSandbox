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

package utils

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

const (
	// AnnotationEndpoints is the annotation key for storing BatchSandbox endpoints
	AnnotationEndpoints = "sandbox.opensandbox.io/endpoints"
)

// GetEndpoints extracts endpoint IPs from BatchSandbox annotations
// Returns a slice of IP addresses parsed from the endpoints annotation
// The annotation format is a JSON array: ["10.244.1.5", "10.244.1.6"]
func GetEndpoints(bs *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	if bs == nil {
		return nil, fmt.Errorf("BatchSandbox is nil")
	}
	bound, err := AllocationMode(bs)
	if err != nil {
		return nil, err
	}
	if bound {
		return nil, fmt.Errorf("UID-bound endpoints require GetEndpointsWithReader")
	}
	return parseEndpoints(bs)
}

func parseEndpoints(bs *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	if bs.Annotations == nil {
		return nil, fmt.Errorf("BatchSandbox has no annotations")
	}

	endpointsAnnotation := bs.Annotations[AnnotationEndpoints]
	if endpointsAnnotation == "" {
		return nil, fmt.Errorf("missing %s annotation", AnnotationEndpoints)
	}

	var endpoints []string
	if err := json.Unmarshal([]byte(endpointsAnnotation), &endpoints); err != nil {
		return nil, fmt.Errorf("failed to parse endpoints annotation: %w", err)
	}

	if len(endpoints) == 0 {
		return nil, fmt.Errorf("endpoints annotation contains no IPs")
	}

	return endpoints, nil
}

// GetEndpointsWithReader preserves native parsing without reads. Protected
// endpoints require an uncached reader and the live, ready, UID-pinned Pod.
func GetEndpointsWithReader(ctx context.Context, reader client.Reader, bs *sandboxv1alpha1.BatchSandbox) ([]string, error) {
	if bs == nil {
		return nil, fmt.Errorf("BatchSandbox is nil")
	}
	bound, err := AllocationMode(bs)
	if err != nil {
		return nil, err
	}
	if !bound {
		return GetEndpoints(bs)
	}
	pod, err := ReadUIDBoundAllocation(ctx, reader, bs)
	if err != nil {
		return nil, err
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	endpoints, err := parseEndpoints(bs)
	if err != nil {
		return nil, err
	}
	if !ready || pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" ||
		len(endpoints) != 1 || endpoints[0] != pod.Status.PodIP {
		return nil, fmt.Errorf("endpoints do not match the ready UID-bound Pod")
	}
	return endpoints, nil
}
