package katavmstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

const maxManifestBytes = 8 << 20
const maxSnapshotBytes int64 = 1 << 40
const restorePlanFile = "opensandbox-restore-plan.json"

type remoteManifest struct {
	Version        int          `json:"version"`
	SnapshotName   string       `json:"snapshotName"`
	RuntimeVersion string       `json:"runtimeVersion"`
	Files          []remoteFile `json:"files"`
}

type remoteFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

func digest(data []byte) string           { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func objectPrefix(name string) string     { return "kata/" + name + "/" }
func manifestKey(name string) string      { return objectPrefix(name) + "manifest.json" }
func payloadKey(name, hash string) string { return objectPrefix(name) + "objects/" + hash + ".zst" }

// uploadSnapshot commits a manifest only after every compressed payload exists.
// The directory must be an immutable, completed kata-ctl snapshot.
func uploadSnapshot(ctx context.Context, store objectStore, root, name, runtimeVersion string) (string, error) {
	if err := validateSnapshotName(name); err != nil {
		return "", err
	}
	manifest := remoteManifest{Version: 1, SnapshotName: name, RuntimeVersion: runtimeVersion}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot contains a link or special file: %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		hash := sha256.New()
		if _, err := io.Copy(hash, f); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		record := remoteFile{Path: filepath.ToSlash(rel), Size: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), Mode: uint32(info.Mode().Perm())}
		manifest.Files = append(manifest.Files, record)
		if err := validateManifest(manifest, false); err != nil {
			return err
		}
		reader, writer := io.Pipe()
		done := make(chan error, 1)
		go func() {
			encoder, err := zstd.NewWriter(writer, zstd.WithEncoderConcurrency(1))
			if err == nil {
				_, err = io.Copy(encoder, f)
				err = errors.Join(err, encoder.Close())
			}
			_ = writer.CloseWithError(err)
			done <- err
		}()
		err = store.put(ctx, payloadKey(name, record.SHA256), reader)
		_ = reader.CloseWithError(err)
		compressionErr := <-done
		// Conditional uploads can stop reading when the object already exists.
		if err != nil {
			return err
		}
		if compressionErr != nil && !errors.Is(compressionErr, io.ErrClosedPipe) {
			return compressionErr
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	if err := validateManifest(manifest, true); err != nil {
		return "", err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	if len(data) > maxManifestBytes {
		return "", errors.New("snapshot manifest too large")
	}
	if err := store.put(ctx, manifestKey(name), bytes.NewReader(data)); err != nil {
		return "", err
	}
	committed, err := readObject(ctx, store, manifestKey(name), maxManifestBytes)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(data, committed) {
		return "", errors.New("remote snapshot already has a different manifest")
	}
	return digest(data), nil
}

func validateManifest(m remoteManifest, complete bool) error {
	if m.Version != 1 || m.RuntimeVersion == "" {
		return errors.New("unsupported or incomplete remote snapshot manifest")
	}
	if err := validateSnapshotName(m.SnapshotName); err != nil {
		return err
	}
	seen := map[string]bool{}
	var total int64
	if len(m.Files) > 10000 {
		return errors.New("too many snapshot files")
	}
	for _, f := range m.Files {
		if f.Path == "." || !filepath.IsLocal(f.Path) || filepath.ToSlash(filepath.Clean(f.Path)) != f.Path || strings.Contains(f.Path, "\\") || seen[f.Path] {
			return fmt.Errorf("unsafe or duplicate snapshot path %q", f.Path)
		}
		if f.Size < 0 || f.Size > maxSnapshotBytes-total || f.Mode > 0777 || len(f.SHA256) != 64 {
			return fmt.Errorf("invalid file metadata for %q", f.Path)
		}
		if _, err := hex.DecodeString(f.SHA256); err != nil {
			return err
		}
		total += f.Size
		seen[f.Path] = true
	}
	if complete {
		for _, required := range []string{SnapshotMetadataFile, "config.json", "memory-ranges", restorePlanFile} {
			if !seen[required] {
				return fmt.Errorf("snapshot missing %s", required)
			}
		}
	}
	return nil
}

func readObject(ctx context.Context, store objectStore, key string, limit int64) ([]byte, error) {
	r, err := store.get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("remote object exceeds size limit")
	}
	return data, err
}

// prepareSnapshot never overwrites an existing cache: an active restored VM may
// still be reading it. Existing files must match the pinned remote manifest.
func prepareSnapshot(ctx context.Context, store objectStore, root, name, expectedDigest string) error {
	if err := validateSnapshotName(name); err != nil {
		return err
	}
	if len(expectedDigest) != 64 {
		return errors.New("pinned manifest digest is required")
	}
	data, err := readObject(ctx, store, manifestKey(name), maxManifestBytes)
	if err != nil {
		return err
	}
	if digest(data) != expectedDigest {
		return errors.New("remote manifest checksum mismatch")
	}
	var manifest remoteManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if err := validateManifest(manifest, true); err != nil {
		return err
	}
	if manifest.SnapshotName != name {
		return errors.New("remote snapshot identity mismatch")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, "."+name+".lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	target := filepath.Join(root, name)
	if _, err := os.Lstat(target); err == nil {
		return verifyCache(target, manifest)
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.MkdirTemp(root, "."+name+"-download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, record := range manifest.Files {
		if err := downloadFile(ctx, store, tmp, name, record); err != nil {
			return err
		}
	}
	if err := verifyCache(tmp, manifest); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func downloadFile(ctx context.Context, store objectStore, root, name string, record remoteFile) error {
	r, err := store.get(ctx, payloadKey(name, record.SHA256))
	if err != nil {
		return err
	}
	defer r.Close()
	decoder, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		return err
	}
	defer decoder.Close()
	path := filepath.Join(root, record.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	reader := io.TeeReader(io.LimitReader(decoder, record.Size+1), hash)
	written, err := copySparse(ctx, f, reader)
	if err != nil {
		return err
	}
	if written != record.Size || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
		return fmt.Errorf("snapshot checksum/size mismatch: %s", record.Path)
	}
	if err := f.Chmod(fs.FileMode(record.Mode)); err != nil {
		return err
	}
	return f.Sync()
}

func copySparse(ctx context.Context, f *os.File, r io.Reader) (int64, error) {
	buffer := make([]byte, 128<<10)
	zero := make([]byte, len(buffer))
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := r.Read(buffer)
		if n > 0 {
			var writeErr error
			if bytes.Equal(buffer[:n], zero[:n]) {
				_, writeErr = f.Seek(int64(n), io.SeekCurrent)
			} else {
				_, writeErr = f.Write(buffer[:n])
			}
			total += int64(n)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if err == io.EOF {
			return total, f.Truncate(total)
		}
		if err != nil {
			return total, err
		}
	}
}

func verifyCache(root string, m remoteManifest) error {
	// Explicitly reject all symlinks, including
	// the root and parent components, even if they resolve inside the tree.
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("snapshot cache is not a directory")
	}
	for _, record := range m.Files {
		path := root
		for _, part := range strings.Split(record.Path, "/") {
			path = filepath.Join(path, part)
			info, err = os.Lstat(path)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("snapshot cache contains a symlink")
			}
		}
		if !info.Mode().IsRegular() || info.Size() != record.Size {
			return fmt.Errorf("invalid cached snapshot file %s", record.Path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
			return fmt.Errorf("cached snapshot checksum mismatch: %s", record.Path)
		}
	}
	return nil
}
