package katavmstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type memoryStore struct {
	objects map[string][]byte
	fail    string
}

func (s *memoryStore) put(_ context.Context, key string, r io.Reader) error {
	if strings.Contains(key, s.fail) && s.fail != "" {
		return errors.New("interrupted upload")
	}
	data, err := io.ReadAll(r)
	if err == nil {
		if _, exists := s.objects[key]; !exists {
			s.objects[key] = data
		}
	}
	return err
}
func (s *memoryStore) get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := s.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
func (s *memoryStore) deletePrefix(_ context.Context, prefix string) error {
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			delete(s.objects, k)
		}
	}
	return nil
}

const remoteTestName = "ks-0123456789abcdef0123456789abcdef"

func remoteFixture(t *testing.T) (string, *memoryStore) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{SnapshotMetadataFile, "config.json", "memory-ranges", restorePlanFile} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(`{"runtime_version":"3.32.0"}`), 0600))
	}
	require.NoError(t, os.Mkdir(filepath.Join(root, "containers"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "containers", "disk.img"), append(make([]byte, 1<<20), []byte("disk data")...), 0600))
	return root, &memoryStore{objects: map[string][]byte{}}
}

func TestRemoteSnapshotRoundTripConcurrentPrepare(t *testing.T) {
	root, store := remoteFixture(t)
	ctx := context.Background()
	hash, err := uploadSnapshot(ctx, store, root, remoteTestName, "3.32.0")
	require.NoError(t, err)
	// Source files are gone. Both restores must obtain their bytes remotely.
	require.NoError(t, os.RemoveAll(root))
	target := t.TempDir()
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- prepareSnapshot(ctx, store, target, remoteTestName, hash) }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	data, err := os.ReadFile(filepath.Join(target, remoteTestName, "containers", "disk.img"))
	require.NoError(t, err)
	require.Equal(t, append(make([]byte, 1<<20), []byte("disk data")...), data)
	// An existing damaged cache must not be overwritten while a VM may use it.
	require.NoError(t, os.WriteFile(filepath.Join(target, remoteTestName, "config.json"), []byte("corrupt"), 0600))
	require.Error(t, prepareSnapshot(ctx, store, target, remoteTestName, hash))
}

func TestRemoteSnapshotInterruptedUploadDoesNotPublish(t *testing.T) {
	root, store := remoteFixture(t)
	store.fail = "/objects/"
	_, err := uploadSnapshot(context.Background(), store, root, remoteTestName, "3.32.0")
	require.Error(t, err)
	require.NotContains(t, store.objects, manifestKey(remoteTestName))
}

func TestRemoteSnapshotRejectsCorruptionAndUnsafePaths(t *testing.T) {
	for _, mutation := range []string{"manifest", "payload", "traversal", "absolute", "duplicate", "symlink"} {
		t.Run(mutation, func(t *testing.T) {
			root, store := remoteFixture(t)
			ctx := context.Background()
			hash, err := uploadSnapshot(ctx, store, root, remoteTestName, "3.32.0")
			require.NoError(t, err)
			target := t.TempDir()
			switch mutation {
			case "manifest":
				store.objects[manifestKey(remoteTestName)] = []byte("corrupt")
			case "payload":
				for k := range store.objects {
					if strings.Contains(k, "/objects/") {
						store.objects[k] = []byte("corrupt")
					}
				}
			case "symlink":
				require.NoError(t, os.Symlink(root, filepath.Join(target, remoteTestName)))
			default:
				var m remoteManifest
				require.NoError(t, json.Unmarshal(store.objects[manifestKey(remoteTestName)], &m))
				if mutation == "traversal" {
					m.Files[0].Path = "../escape"
				}
				if mutation == "absolute" {
					m.Files[0].Path = "/etc/escape"
				}
				if mutation == "duplicate" {
					m.Files = append(m.Files, m.Files[0])
				}
				data, err := json.Marshal(m)
				require.NoError(t, err)
				store.objects[manifestKey(remoteTestName)] = data
				hash = digest(data)
			}
			require.Error(t, prepareSnapshot(ctx, store, target, remoteTestName, hash))
			if mutation != "symlink" {
				_, err := os.Stat(filepath.Join(target, remoteTestName))
				require.True(t, os.IsNotExist(err))
			}
		})
	}
}

func TestRemoteSnapshotRejectsLinksAndMissingArtifacts(t *testing.T) {
	root, store := remoteFixture(t)
	require.NoError(t, os.Symlink("config.json", filepath.Join(root, "link")))
	_, err := uploadSnapshot(context.Background(), store, root, remoteTestName, "3.32.0")
	require.Error(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, "link")))
	require.NoError(t, os.Remove(filepath.Join(root, "memory-ranges")))
	_, err = uploadSnapshot(context.Background(), store, root, remoteTestName, "3.32.0")
	require.Error(t, err)
	require.NotContains(t, store.objects, manifestKey(remoteTestName))
}

func TestBlobRejectsCredentialURLs(t *testing.T) {
	for _, account := range []string{"http://account.blob.core.windows.net", "https://account.blob.core.windows.net?sig=secret", "https://user:pass@account.blob.core.windows.net", "https://account.blob.core.windows.net/container"} {
		_, err := newBlobStore(account, "snapshots")
		require.Error(t, err)
	}
}
