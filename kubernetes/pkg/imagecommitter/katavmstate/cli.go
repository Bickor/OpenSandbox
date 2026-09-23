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
	"strings"

	containerd "github.com/containerd/containerd"
)

// Run executes the trusted same-node Kata VM state worker mode.
func Run(ctx context.Context, args []string, terminationMessagePath string, output io.Writer) error {
	operation, request, snapshotName, err := parseArgs(args)
	if err != nil {
		return err
	}
	var result Result
	if operation == "delete" {
		result, err = (&worker{
			hostRoot:     HostRoot,
			snapshotRoot: SnapshotRoot,
			removeAll:    os.RemoveAll,
		}).delete(snapshotName)
	} else {
		client, clientErr := containerd.New(
			containerdSocket(),
			containerd.WithDefaultNamespace(containerdNamespace),
		)
		if clientErr != nil {
			return fmt.Errorf("connect to containerd: %w", clientErr)
		}
		defer client.Close()
		result, err = newWorker(client).create(ctx, request)
	}
	if err != nil {
		return err
	}
	if err := writeResult(terminationMessagePath, result); err != nil {
		return fmt.Errorf("write Kata VM state result: %w", err)
	}
	if output != nil {
		fmt.Fprintf(output, "KATA_VMSTATE_SNAPSHOT_NAME=%s\n", result.KataVMState.SnapshotName)
		if result.KataVMState.RuntimeVersion != "" {
			fmt.Fprintf(output, "KATA_VMSTATE_RUNTIME_VERSION=%s\n", result.KataVMState.RuntimeVersion)
		}
	}
	return nil
}

func parseArgs(args []string) (string, createRequest, string, error) {
	if len(args) < 2 || args[0] != "kata-vmstate" {
		return "", createRequest{}, "", errors.New("usage: image-committer kata-vmstate <create|delete> [flags]")
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
	case "delete":
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
		return "delete", createRequest{}, snapshotName, nil
	default:
		return "", createRequest{}, "", fmt.Errorf("unsupported kata-vmstate operation %q", args[1])
	}
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
