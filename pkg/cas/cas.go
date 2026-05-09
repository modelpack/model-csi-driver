// Package cas implements a node-local content-addressable store for model
// layer blobs. Blobs are deduplicated on disk via per-digest hardlinks and
// reference-counted by owner.
//
// API: EnsureLink claims a ref and hardlinks the blob if cached; Import
// publishes a freshly downloaded blob; ReleaseLayer / ReleaseAll drop refs
// (and the blob when the count hits zero).
//
// Concurrency: when several owners miss the same digest, exactly one becomes
// the leader (hit=false) and the rest block until it Imports (success) or
// AbortDownload (failure). This dedupes both storage and download bandwidth.
package cas

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/containerd/pkg/kmutex"
	"github.com/pkg/errors"
)

// inflightDownload tracks an in-progress download. done is closed when the
// leader finishes (Import or AbortDownload), waking all followers.
type inflightDownload struct {
	done chan struct{}
}

// Store is a node-local CAS store.
type Store struct {
	root string
	km   kmutex.KeyedLocker

	mu       sync.Mutex
	inflight map[string]*inflightDownload
}

// NewStore opens (or creates) a CAS store at root and removes stale tmp files.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("cas: empty root")
	}
	if err := os.MkdirAll(blobsRoot(root), 0o755); err != nil {
		return nil, errors.Wrap(err, "create cas blobs dir")
	}
	s := &Store{root: root, km: kmutex.New(), inflight: make(map[string]*inflightDownload)}
	if err := s.cleanupStaleTmp(); err != nil {
		return nil, errors.Wrap(err, "cleanup stale tmp")
	}
	return s, nil
}

// Root returns the CAS root directory.
func (s *Store) Root() string { return s.root }

// blobsRoot returns "<root>/blobs/sha256".
func blobsRoot(root string) string { return filepath.Join(root, "blobs", "sha256") }

// blobPath is the canonical file for a digest (single regular file, no
// subdirectories, so hardlinking is trivial).
func (s *Store) blobPath(digest string) string {
	return filepath.Join(blobsRoot(s.root), encodeDigest(digest))
}

// refsDir is the per-digest directory holding one empty file per owner ref.
func (s *Store) refsDir(digest string) string {
	return filepath.Join(blobsRoot(s.root), encodeDigest(digest)+".refs")
}

// tmpPathFor returns a unique <digest>.tmp.<rand> path for staged writes.
func (s *Store) tmpPathFor(digest string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return filepath.Join(blobsRoot(s.root), encodeDigest(digest)+".tmp."+hex.EncodeToString(buf[:])), nil
}

// encodeDigest strips the "sha256:" prefix; the algorithm is implied by the
// parent dir (blobs/sha256/).
func encodeDigest(digest string) string {
	if i := strings.Index(digest, ":"); i >= 0 {
		return digest[i+1:]
	}
	return digest
}

// decodeDigestFromRefsName extracts the digest from a "<hex>.refs" entry.
// Returns "" if name doesn't carry the .refs suffix.
func decodeDigestFromRefsName(name string) string {
	trimmed := strings.TrimSuffix(name, ".refs")
	if trimmed == "" || trimmed == name {
		return ""
	}
	return "sha256:" + trimmed
}

// cleanupStaleTmp removes leftover .tmp / .tmp.* files from a previous crash.
func (s *Store) cleanupStaleTmp() error {
	entries, err := os.ReadDir(blobsRoot(s.root))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, ".tmp.") || strings.HasSuffix(name, ".tmp") {
			_ = os.RemoveAll(filepath.Join(blobsRoot(s.root), name))
		}
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isEmptyDir(p string) (bool, error) {
	entries, err := os.ReadDir(p)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// EnsureLink claims a ref for ownerKey on digest and, on cache hit, hardlinks
// the blob to dstPath (created if needed; existing file replaced; may be "").
//
// Returns:
//   - hit=true: blob present, caller should skip its download.
//   - hit=false: caller is elected leader; MUST call Import (on success) or
//     AbortDownload (on failure) to release blocked followers.
//
// If another owner is already downloading, the call blocks until that
// download finishes, then re-evaluates (typically returning hit=true).
// On any return path, a ref is recorded so the blob cannot be purged from
// under the caller.
func (s *Store) EnsureLink(ctx context.Context, ownerKey, digest, dstPath string) (hit bool, err error) {
	if digest == "" {
		return false, errors.New("cas: empty digest")
	}
	if ownerKey == "" {
		return false, errors.New("cas: empty owner key")
	}
	for {
		if err := s.km.Lock(ctx, digest); err != nil {
			return false, errors.Wrapf(err, "lock digest: %s", digest)
		}

		if err := s.addRefLocked(digest, ownerKey); err != nil {
			s.km.Unlock(digest)
			return false, errors.Wrap(err, "add ref")
		}

		// Cache hit.
		if fileExists(s.blobPath(digest)) {
			s.km.Unlock(digest)
			if dstPath == "" {
				return true, nil
			}
			if err := s.linkInto(s.blobPath(digest), dstPath); err != nil {
				return true, errors.Wrap(err, "hardlink to destination")
			}
			return true, nil
		}

		// Cache miss: join an in-progress download or become the leader.
		s.mu.Lock()
		if d, ok := s.inflight[digest]; ok {
			wait := d.done
			s.mu.Unlock()
			// Drop the per-digest lock so the leader can progress.
			s.km.Unlock(digest)
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				// Drop our ref so we don't leak it.
				_ = s.ReleaseLayer(context.Background(), digest, ownerKey)
				return false, ctx.Err()
			}
		}
		s.inflight[digest] = &inflightDownload{done: make(chan struct{})}
		s.mu.Unlock()
		s.km.Unlock(digest)
		return false, nil
	}
}

// finishDownload removes the in-flight slot and wakes all followers.
// No-op if no slot exists.
func (s *Store) finishDownload(digest string) {
	s.mu.Lock()
	d, ok := s.inflight[digest]
	if ok {
		delete(s.inflight, digest)
	}
	s.mu.Unlock()
	if ok {
		close(d.done)
	}
}

// AbortDownload is called by a failed leader to wake followers; one of them
// will be elected as the new leader. The leader's ref is left intact (drop
// it explicitly via ReleaseLayer / ReleaseAll). Safe to call with no slot.
func (s *Store) AbortDownload(digest string) {
	s.finishDownload(digest)
}

func (s *Store) addRefLocked(digest, ownerKey string) error {
	dir := s.refsDir(digest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ownerKey), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// linkInto hardlinks src to dst, creating parent dirs and replacing any
// existing file at dst.
func (s *Store) linkInto(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, statErr := os.Lstat(dst); statErr == nil {
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	return os.Link(src, dst)
}

// Import publishes srcPath as the canonical blob for digest, then replaces
// srcPath with a hardlink to it. The caller must have previously called
// EnsureLink (so a ref exists). On race, late importers fall back to linking
// against the winner's blob. Import always wakes followers blocked in
// EnsureLink, on both success and failure paths.
func (s *Store) Import(ctx context.Context, digest, srcPath string) error {
	if digest == "" {
		return errors.New("cas: empty digest")
	}
	if !fileExists(srcPath) {
		return errors.Errorf("cas: source file missing: %s", srcPath)
	}
	if err := s.km.Lock(ctx, digest); err != nil {
		return errors.Wrapf(err, "lock digest: %s", digest)
	}
	defer s.km.Unlock(digest)
	defer s.finishDownload(digest)

	final := s.blobPath(digest)
	if fileExists(final) {
		// Lost the import race: discard our copy and link the winner's.
		return s.linkInto(final, srcPath)
	}

	tmp, err := s.tmpPathFor(digest)
	if err != nil {
		return errors.Wrap(err, "make tmp path")
	}
	// Stage src -> tmp -> final so publish is an atomic rename within blobs/.
	if err := os.Rename(srcPath, tmp); err != nil {
		return errors.Wrapf(err, "move src into cas tmp: %s -> %s", srcPath, tmp)
	}
	if err := os.Rename(tmp, final); err != nil {
		if fileExists(final) {
			// Rename race lost: link the winner's blob.
			_ = os.Remove(tmp)
			return s.linkInto(final, srcPath)
		}
		// Best effort: restore srcPath so the caller still has its extract.
		_ = os.Rename(tmp, srcPath)
		return errors.Wrapf(err, "publish blob: %s", digest)
	}
	// Materialize the canonical blob back at the extract path.
	return s.linkInto(final, srcPath)
}

// ReleaseLayer drops ownerKey's ref for digest, deleting the blob when the
// ref count hits zero.
func (s *Store) ReleaseLayer(ctx context.Context, digest, ownerKey string) error {
	if digest == "" || ownerKey == "" {
		return errors.New("cas: empty digest or owner")
	}
	if err := s.km.Lock(ctx, digest); err != nil {
		return errors.Wrapf(err, "lock digest: %s", digest)
	}
	defer s.km.Unlock(digest)
	return s.releaseLocked(digest, ownerKey)
}

func (s *Store) releaseLocked(digest, ownerKey string) error {
	refsDir := s.refsDir(digest)
	if err := os.Remove(filepath.Join(refsDir, ownerKey)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Wrap(err, "remove ref")
	}
	empty, err := isEmptyDir(refsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			_ = os.RemoveAll(s.blobPath(digest))
			return nil
		}
		return errors.Wrap(err, "check refs dir")
	}
	if !empty {
		return nil
	}
	if err := os.RemoveAll(s.blobPath(digest)); err != nil {
		return errors.Wrap(err, "remove blob")
	}
	if err := os.RemoveAll(refsDir); err != nil {
		return errors.Wrap(err, "remove refs dir")
	}
	return nil
}

// ReleaseAll drops every ref held by ownerKey across all digests. Safe even
// when the owner holds no refs.
func (s *Store) ReleaseAll(ctx context.Context, ownerKey string) error {
	if ownerKey == "" {
		return errors.New("cas: empty owner")
	}
	entries, err := os.ReadDir(blobsRoot(s.root))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return errors.Wrap(err, "read blobs dir")
	}
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".refs") {
			continue
		}
		digest := decodeDigestFromRefsName(name)
		if digest == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(blobsRoot(s.root), name, ownerKey)); err != nil {
			continue
		}
		if err := s.ReleaseLayer(ctx, digest, ownerKey); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Has reports whether the blob is present in the CAS.
func (s *Store) Has(digest string) bool { return fileExists(s.blobPath(digest)) }

// RefCount returns the current reference count for digest.
func (s *Store) RefCount(digest string) (int, error) {
	entries, err := os.ReadDir(s.refsDir(digest))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return len(entries), nil
}

// OwnerKey encodes (volumeName, mountID) as a single ref filename. When
// mountID is empty the key is just volumeName; otherwise the two are joined
// by "__" (K8s PVC names cannot contain underscores, so no collision).
func OwnerKey(volumeName, mountID string) string {
	if mountID == "" {
		return volumeName
	}
	return fmt.Sprintf("%s__%s", volumeName, mountID)
}
