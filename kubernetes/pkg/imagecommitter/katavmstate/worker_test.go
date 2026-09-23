// Copyright 2026 Alibaba Group Holding Ltd.
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

package katavmstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const testSnapshotName = "ks-0123456789abcdef0123456789abcdef"

func TestSelectPauseContainerRequiresExactlyOneMatch(t *testing.T) {
	request := createRequest{PodName: "pod", PodNamespace: "tenant", PodUID: "uid"}
	matching := containerMetadata{ID: "sandbox-id", Labels: map[string]string{
		podNameLabel: "pod", podNamespaceLabel: "tenant", podUIDLabel: "uid", containerNameLabel: PauseContainerName,
	}}
	nonPause := containerMetadata{ID: "workload", Labels: map[string]string{
		podNameLabel: "pod", podNamespaceLabel: "tenant", podUIDLabel: "uid", containerNameLabel: "main",
	}}

	id, err := selectPauseContainer([]containerMetadata{nonPause, matching}, request)
	if err != nil || id != "sandbox-id" {
		t.Fatalf("selectPauseContainer() = %q, %v", id, err)
	}
	if _, err := selectPauseContainer(nil, request); err == nil {
		t.Fatal("zero matches unexpectedly succeeded")
	}
	if _, err := selectPauseContainer([]containerMetadata{matching, matching}, request); err == nil {
		t.Fatal("multiple matches unexpectedly succeeded")
	}
}

func TestWorkerCreateInvokesChrootedKataCtlAndReadsRuntimeVersion(t *testing.T) {
	hostRoot := t.TempDir()
	metadataPath := filepath.Join(hostRoot, "var/lib/kata/snapshots", testSnapshotName, SnapshotMetadataFile)
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(`{"runtime_version":"3.15.0-aks.1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotRoot, gotName string
	var gotArgs []string
	w := &worker{
		hostRoot:     hostRoot,
		snapshotRoot: SnapshotRoot,
		listContainers: func(context.Context) ([]containerMetadata, error) {
			return []containerMetadata{{ID: "sandbox-id", Labels: map[string]string{
				podNameLabel: "pod", podNamespaceLabel: "tenant", podUIDLabel: "uid", containerNameLabel: PauseContainerName,
			}}}, nil
		},
		runChild: func(_ context.Context, root, name string, args ...string) ([]byte, error) {
			gotRoot, gotName, gotArgs = root, name, append([]string(nil), args...)
			return nil, nil
		},
		readFile: os.ReadFile,
	}
	result, err := w.create(context.Background(), createRequest{
		PodName: "pod", PodNamespace: "tenant", PodUID: "uid", SnapshotName: testSnapshotName,
		HostKataCtlPath: DefaultKataCtlPath,
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if gotRoot != hostRoot || gotName != DefaultKataCtlPath {
		t.Fatalf("unexpected child target root=%q name=%q", gotRoot, gotName)
	}
	wantArgs := []string{"snapshot", "create", "--sandbox-id", "sandbox-id", "--path", SnapshotRoot + "/" + testSnapshotName}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("child args = %v, want %v", gotArgs, wantArgs)
	}
	if result.KataVMState.SnapshotName != testSnapshotName || result.KataVMState.RuntimeVersion != "3.15.0-aks.1" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestWorkerDeleteIsScopedAndIdempotent(t *testing.T) {
	hostRoot := t.TempDir()
	target := filepath.Join(hostRoot, "var/lib/kata/snapshots", testSnapshotName)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	w := &worker{hostRoot: hostRoot, snapshotRoot: SnapshotRoot, removeAll: os.RemoveAll}
	for i := 0; i < 2; i++ {
		result, err := w.delete(testSnapshotName)
		if err != nil {
			t.Fatalf("delete attempt %d failed: %v", i+1, err)
		}
		if result.KataVMState.SnapshotName != testSnapshotName {
			t.Fatalf("unexpected delete result: %#v", result)
		}
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot directory still exists: %v", err)
	}
	for _, invalid := range []string{"", "../escape", "ks-ABCDEF0123456789abcdef0123456789", "ks-short"} {
		if _, err := w.delete(invalid); err == nil {
			t.Fatalf("invalid snapshot name %q unexpectedly succeeded", invalid)
		}
	}
}

func TestParseArgsRejectsUnexpectedInput(t *testing.T) {
	if operation, request, _, err := parseArgs([]string{
		"kata-vmstate", "create", "--pod-name", "pod", "--pod-namespace", "tenant", "--pod-uid", "uid", "--snapshot-name", testSnapshotName,
	}); err != nil || operation != "create" || request.HostKataCtlPath != DefaultKataCtlPath {
		t.Fatalf("parse create = %q, %#v, %v", operation, request, err)
	}
	for _, args := range [][]string{
		nil,
		{"kata-vmstate"},
		{"kata-vmstate", "unknown"},
		{"kata-vmstate", "delete", "--snapshot-name", testSnapshotName, "extra"},
	} {
		if _, _, _, err := parseArgs(args); err == nil {
			t.Fatalf("parseArgs(%v) unexpectedly succeeded", args)
		}
	}
}

func TestResultJSONIsStable(t *testing.T) {
	data, err := json.Marshal(Result{Containers: []struct{}{}, KataVMState: VMStateResult{
		SnapshotName: testSnapshotName, RuntimeVersion: "3.15.0-aks.1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"containers":[],"kataVMState":{"snapshotName":"ks-0123456789abcdef0123456789abcdef","runtimeVersion":"3.15.0-aks.1"}}`
	if string(data) != want {
		t.Fatalf("result JSON = %s, want %s", data, want)
	}
}
