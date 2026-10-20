package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/snapshot"
)

// Reuse the standard QEMU worker, supplying its registry authentication through
// a private temporary Docker config instead of command arguments or log output.
func runQEMUWorker(ctx context.Context, args []string) error {
	// An explicitly mounted registry Secret takes precedence, matching the
	// controller's snapshotPushSecret contract (useful without Azure RBAC writes).
	dir, err := os.MkdirTemp("", "qemu-registry-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if data, err := os.ReadFile("/var/run/opensandbox/registry/config.json"); err == nil {
		// nerdctl login updates config.json; never use a read-only Secret mount
		// as DOCKER_CONFIG directly.
		if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
			return err
		}
		return executeQEMUWorker(ctx, args, dir)
	}
	if args[0] == "snapshot" {
		if len(args) != 3 || args[1] != "--request-base64" {
			return fmt.Errorf("invalid QEMU snapshot arguments")
		}
		data, err := base64.StdEncoding.DecodeString(args[2])
		if err != nil {
			return err
		}
		var request snapshot.Request
		if err := json.Unmarshal(data, &request); err != nil {
			return err
		}
		provider, err := newACRCredentialProvider()
		if err != nil {
			return err
		}
		auths := map[string]map[string]string{}
		images := []string{request.VMStateImageURI}
		for _, container := range request.Containers {
			images = append(images, container.ImageURI)
		}
		for _, image := range images {
			host := strings.SplitN(image, "/", 2)[0]
			if _, exists := auths[host]; exists {
				continue
			}
			credential, err := provider.Credential(ctx, host)
			if err != nil {
				return err
			}
			auths[host] = map[string]string{"username": "00000000-0000-0000-0000-000000000000", "password": credential.RefreshToken}
		}
		data, err = json.Marshal(map[string]any{"auths": auths})
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
			return err
		}
	}
	return executeQEMUWorker(ctx, args, dir)
}

func executeQEMUWorker(ctx context.Context, args []string, dir string) error {
	cmd := exec.Command("/usr/local/bin/image-committer-qemu", args...)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+dir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case <-done:
		}
	}()
	return cmd.Wait()
}
