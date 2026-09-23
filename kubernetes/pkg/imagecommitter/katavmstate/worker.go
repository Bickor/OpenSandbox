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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	containerd "github.com/containerd/containerd"
	"github.com/containerd/errdefs"
)

const (
	HostRoot              = "/host"
	SnapshotRoot          = "/var/lib/kata/snapshots"
	DefaultKataCtlPath    = "/opt/aks-sandbox-demo/kata-v2/bin/kata-ctl"
	SnapshotMetadataFile  = "kata-snapshot.json"
	PauseContainerName    = "POD"
	defaultContainerdSock = "/run/containerd/containerd.sock"
	containerdNamespace   = "k8s.io"

	podNameLabel       = "io.kubernetes.pod.name"
	podNamespaceLabel  = "io.kubernetes.pod.namespace"
	podUIDLabel        = "io.kubernetes.pod.uid"
	containerNameLabel = "io.kubernetes.container.name"
)

var snapshotNamePattern = regexp.MustCompile(`^ks-[0-9a-f]{32}$`)

// Result is written to the Kubernetes termination message by both operations.
type Result struct {
	Containers  []struct{}    `json:"containers"`
	KataVMState VMStateResult `json:"kataVMState"`
}

// VMStateResult identifies the node-local Kata snapshot.
type VMStateResult struct {
	SnapshotName   string `json:"snapshotName"`
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
}

type createRequest struct {
	PodName         string
	PodNamespace    string
	PodUID          string
	SnapshotName    string
	HostKataCtlPath string
}

type containerMetadata struct {
	ID     string
	Labels map[string]string
}

type worker struct {
	hostRoot       string
	snapshotRoot   string
	listContainers func(context.Context) ([]containerMetadata, error)
	runChild       func(context.Context, string, string, ...string) ([]byte, error)
	readFile       func(string) ([]byte, error)
	removeAll      func(string) error
}

func newWorker(client *containerd.Client) *worker {
	return &worker{
		hostRoot:     HostRoot,
		snapshotRoot: SnapshotRoot,
		listContainers: func(ctx context.Context) ([]containerMetadata, error) {
			containers, err := client.Containers(ctx)
			if err != nil {
				return nil, fmt.Errorf("list containerd containers: %w", err)
			}
			result := make([]containerMetadata, 0, len(containers))
			for _, container := range containers {
				info, err := container.Info(ctx)
				if err != nil {
					if errdefs.IsNotFound(err) {
						continue
					}
					return nil, fmt.Errorf("inspect container %s: %w", container.ID(), err)
				}
				result = append(result, containerMetadata{ID: container.ID(), Labels: info.Labels})
			}
			return result, nil
		},
		runChild:  runChrootedChild,
		readFile:  os.ReadFile,
		removeAll: os.RemoveAll,
	}
}

func (w *worker) create(ctx context.Context, request createRequest) (Result, error) {
	if strings.TrimSpace(request.PodName) == "" || strings.TrimSpace(request.PodNamespace) == "" || strings.TrimSpace(request.PodUID) == "" {
		return Result{}, errors.New("pod name, namespace, and UID are required")
	}
	if err := validateSnapshotName(request.SnapshotName); err != nil {
		return Result{}, err
	}
	if err := validateAbsoluteCleanPath("host root", w.hostRoot); err != nil {
		return Result{}, err
	}
	if w.snapshotRoot != SnapshotRoot {
		return Result{}, fmt.Errorf("snapshot root must be %s", SnapshotRoot)
	}
	if err := validateAbsoluteCleanPath("kata-ctl path", request.HostKataCtlPath); err != nil {
		return Result{}, err
	}

	containers, err := w.listContainers(ctx)
	if err != nil {
		return Result{}, err
	}
	sandboxID, err := selectPauseContainer(containers, request)
	if err != nil {
		return Result{}, err
	}
	snapshotPath := filepath.Join(SnapshotRoot, request.SnapshotName)
	output, err := w.runChild(ctx, w.hostRoot, request.HostKataCtlPath,
		"snapshot", "create", "--sandbox-id", sandboxID, "--path", snapshotPath)
	if err != nil {
		return Result{}, fmt.Errorf("kata-ctl snapshot create failed: %w; output: %s", err, strings.TrimSpace(string(output)))
	}

	metadataPath, err := w.hostSnapshotPath(request.SnapshotName, SnapshotMetadataFile)
	if err != nil {
		return Result{}, err
	}
	data, err := w.readFile(metadataPath)
	if err != nil {
		return Result{}, fmt.Errorf("read Kata snapshot metadata: %w", err)
	}
	var metadata struct {
		RuntimeVersion string `json:"runtime_version"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return Result{}, fmt.Errorf("parse Kata snapshot metadata: %w", err)
	}
	if strings.TrimSpace(metadata.RuntimeVersion) == "" {
		return Result{}, errors.New("Kata snapshot metadata has no runtime_version")
	}
	return Result{Containers: []struct{}{}, KataVMState: VMStateResult{
		SnapshotName:   request.SnapshotName,
		RuntimeVersion: metadata.RuntimeVersion,
	}}, nil
}

func (w *worker) delete(snapshotName string) (Result, error) {
	if err := validateSnapshotName(snapshotName); err != nil {
		return Result{}, err
	}
	if err := validateAbsoluteCleanPath("host root", w.hostRoot); err != nil {
		return Result{}, err
	}
	if w.snapshotRoot != SnapshotRoot {
		return Result{}, fmt.Errorf("snapshot root must be %s", SnapshotRoot)
	}
	target, err := w.hostSnapshotPath(snapshotName)
	if err != nil {
		return Result{}, err
	}
	if err := w.removeAll(target); err != nil && !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("delete Kata snapshot: %w", err)
	}
	return Result{Containers: []struct{}{}, KataVMState: VMStateResult{SnapshotName: snapshotName}}, nil
}

func (w *worker) hostSnapshotPath(snapshotName string, children ...string) (string, error) {
	if err := validateSnapshotName(snapshotName); err != nil {
		return "", err
	}
	relativeRoot := strings.TrimPrefix(SnapshotRoot, string(filepath.Separator))
	parts := append([]string{w.hostRoot, relativeRoot, snapshotName}, children...)
	target := filepath.Join(parts...)
	expectedRoot := filepath.Join(w.hostRoot, relativeRoot)
	if filepath.Dir(filepath.Join(expectedRoot, snapshotName)) != expectedRoot {
		return "", errors.New("snapshot path escaped the fixed root")
	}
	return target, nil
}

func selectPauseContainer(containers []containerMetadata, request createRequest) (string, error) {
	var matches []string
	for _, container := range containers {
		labels := container.Labels
		if labels[podNameLabel] == request.PodName &&
			labels[podNamespaceLabel] == request.PodNamespace &&
			labels[podUIDLabel] == request.PodUID &&
			labels[containerNameLabel] == PauseContainerName {
			matches = append(matches, container.ID)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one CRI pause container for pod %s/%s UID %s, found %d", request.PodNamespace, request.PodName, request.PodUID, len(matches))
	}
	if strings.TrimSpace(matches[0]) == "" {
		return "", errors.New("matched CRI pause container has an empty ID")
	}
	return matches[0], nil
}

func validateSnapshotName(name string) error {
	if !snapshotNamePattern.MatchString(name) || filepath.Base(name) != name || filepath.Clean(name) != name {
		return fmt.Errorf("invalid Kata snapshot name %q", name)
	}
	return nil
}

func validateAbsoluteCleanPath(label, value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return fmt.Errorf("%s must be a clean absolute path", label)
	}
	return nil
}

func runChrootedChild(ctx context.Context, root, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
	return cmd.CombinedOutput()
}
