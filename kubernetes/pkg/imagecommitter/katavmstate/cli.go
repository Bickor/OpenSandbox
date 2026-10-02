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
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	containerd "github.com/containerd/containerd"
)

// Run executes the trusted same-node Kata VM state worker mode.
func Run(ctx context.Context, args []string, terminationMessagePath string, output io.Writer) error {
	operation, request, snapshotName, err := parseArgs(args)
	if err != nil {
		return err
	}
	var result Result
	var timings captureTimings
	started := time.Now()
	completed := false
	if operation == "create" && output != nil {
		defer func() { writeCaptureTiming(output, timings, time.Since(started), completed) }()
	}
	var store objectStore
	account, container := os.Getenv("KATA_SNAPSHOT_BLOB_ACCOUNT_URL"), os.Getenv("KATA_SNAPSHOT_BLOB_CONTAINER")
	if account != "" || container != "" {
		store, err = newBlobStore(account, container)
		if err != nil {
			return err
		}
	}
	if operation == "prepare" {
		if store == nil {
			return errors.New("remote snapshot restore requires Blob storage configuration")
		}
		err = prepareSnapshot(ctx, store, filepath.Join(HostRoot, SnapshotRoot), snapshotName, os.Getenv("KATA_SNAPSHOT_MANIFEST_DIGEST"))
		result = Result{Containers: []struct{}{}, KataVMState: VMStateResult{SnapshotName: snapshotName}}
	} else if operation == "delete-remote" {
		if err := validateSnapshotName(snapshotName); err != nil {
			return err
		}
		if store == nil {
			return errors.New("remote deletion requires Blob storage configuration")
		}
		err = store.deletePrefix(ctx, objectPrefix(snapshotName))
		result = Result{Containers: []struct{}{}, KataVMState: VMStateResult{SnapshotName: snapshotName}}
	} else if operation == "delete" {
		if err := validateSnapshotName(snapshotName); err != nil {
			return err
		}
		if store != nil {
			if err := store.deletePrefix(ctx, objectPrefix(snapshotName)); err != nil {
				return err
			}
		}
		result, err = (&worker{
			hostRoot:     HostRoot,
			snapshotRoot: SnapshotRoot,
			removeAll:    os.RemoveAll,
		}).delete(snapshotName)
	} else {
		connectStart := time.Now()
		client, clientErr := containerd.New(
			containerdSocket(),
			containerd.WithDefaultNamespace(containerdNamespace),
		)
		timings.containerdConnect = time.Since(connectStart)
		if clientErr != nil {
			return fmt.Errorf("connect to containerd: %w", clientErr)
		}
		defer client.Close()
		w := newWorker(client)
		w.timings = &timings
		result, err = w.create(ctx, request)
		if err == nil && store != nil {
			uploadStart := time.Now()
			root := filepath.Join(HostRoot, SnapshotRoot, request.SnapshotName)
			plan, planErr := os.ReadFile("/restore-plan/pod-template.json")
			if planErr != nil {
				timings.remoteUpload = time.Since(uploadStart)
				return fmt.Errorf("read private restore plan: %w", planErr)
			}
			if err := os.WriteFile(filepath.Join(root, restorePlanFile), plan, 0600); err != nil {
				timings.remoteUpload = time.Since(uploadStart)
				return err
			}
			result.KataVMState.ManifestDigest, err = uploadSnapshot(ctx, store, root, request.SnapshotName, result.KataVMState.RuntimeVersion)
			timings.remoteUpload = time.Since(uploadStart)
		}
	}
	if err != nil {
		return err
	}
	writeStart := time.Now()
	err = writeResult(terminationMessagePath, result)
	timings.resultWrite = time.Since(writeStart)
	if err != nil {
		return fmt.Errorf("write Kata VM state result: %w", err)
	}
	if output != nil {
		fmt.Fprintf(output, "KATA_VMSTATE_SNAPSHOT_NAME=%s\n", result.KataVMState.SnapshotName)
		if result.KataVMState.RuntimeVersion != "" {
			fmt.Fprintf(output, "KATA_VMSTATE_RUNTIME_VERSION=%s\n", result.KataVMState.RuntimeVersion)
		}
	}
	completed = true
	return nil
}

func parseArgs(args []string) (string, createRequest, string, error) {
	if len(args) < 2 || args[0] != "kata-vmstate" {
		return "", createRequest{}, "", errors.New("usage: image-committer kata-vmstate <create|prepare|delete|delete-remote> [flags]")
	}
	switch args[1] {
	case "create":
		flags := flag.NewFlagSet("kata-vmstate create", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		request := createRequest{}
		flags.StringVar(&request.PodName, "pod-name", "", "source Pod name")
		flags.StringVar(&request.PodNamespace, "pod-namespace", "", "source Pod namespace")
		flags.StringVar(&request.PodUID, "pod-uid", "", "source Pod UID")
		flags.StringVar(&request.SnapshotName, "snapshot-name", "", "opaque snapshot name")
		flags.StringVar(&request.HostKataCtlPath, "kata-ctl-path", DefaultKataCtlPath, "host kata-ctl path")
		if err := flags.Parse(args[2:]); err != nil {
			return "", createRequest{}, "", err
		}
		if flags.NArg() != 0 {
			return "", createRequest{}, "", errors.New("kata-vmstate create does not accept positional arguments")
		}
		return "create", request, "", nil
	case "delete", "delete-remote", "prepare":
		flags := flag.NewFlagSet("kata-vmstate delete", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		var snapshotName string
		flags.StringVar(&snapshotName, "snapshot-name", "", "opaque snapshot name")
		if err := flags.Parse(args[2:]); err != nil {
			return "", createRequest{}, "", err
		}
		if flags.NArg() != 0 {
			return "", createRequest{}, "", errors.New("kata-vmstate delete does not accept positional arguments")
		}
		return args[1], createRequest{}, snapshotName, nil
	default:
		return "", createRequest{}, "", fmt.Errorf("unsupported kata-vmstate operation %q", args[1])
	}
}

// writeCaptureTiming emits a single metadata-only record; durations include
// failed phases, and zero means a phase did not run or took less than 1 ms.
func writeCaptureTiming(output io.Writer, timings captureTimings, total time.Duration, completed bool) {
	status := "error"
	if completed {
		status = "ok"
	}
	record := struct {
		Operation           string `json:"operation"`
		Status              string `json:"status"`
		TotalMS             int64  `json:"total_ms"`
		ContainerdConnectMS int64  `json:"containerd_connect_ms"`
		SourceLookupMS      int64  `json:"source_lookup_ms"`
		KataCTLSnapshotMS   int64  `json:"kata_ctl_snapshot_ms"`
		MetadataVerifyMS    int64  `json:"metadata_verify_ms"`
		RemoteUploadMS      int64  `json:"remote_upload_ms"`
		ResultWriteMS       int64  `json:"result_write_ms"`
	}{"create", status, total.Milliseconds(), timings.containerdConnect.Milliseconds(),
		timings.sourceLookup.Milliseconds(), timings.snapshotCreate.Milliseconds(),
		timings.metadataVerify.Milliseconds(), timings.remoteUpload.Milliseconds(),
		timings.resultWrite.Milliseconds()}
	data, err := json.Marshal(record)
	if err != nil {
		fmt.Fprintf(output, "KATA_VMSTATE_TIMING_ERROR=%v\n", err)
		return
	}
	fmt.Fprintf(output, "KATA_VMSTATE_TIMING=%s\n", data)
}

func writeResult(path string, result Result) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("termination message path is required")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func containerdSocket() string {
	if value := strings.TrimSpace(os.Getenv("CONTAINERD_SOCKET")); value != "" {
		return value
	}
	return defaultContainerdSock
}
