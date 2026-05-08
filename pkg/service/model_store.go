package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/pkg/kmutex"
	"github.com/pkg/errors"
)

// fs* indirections allow tests to inject filesystem errors.
var (
	fsRemoveAll = os.RemoveAll
	fsMkdirAll  = os.MkdirAll
	fsRename    = os.Rename
)

// ModelStore is a node-local, content-addressed shared cache for model
// artifacts, keyed by manifest digest. Volumes consume entries via
// hardlinks so the on-disk footprint is paid once per digest.
//
// Layout under <rootDir>:
//
//	store/<algo>/<hex>/data/         cached payload (volumes hardlink from here)
//	store/<algo>/<hex>/data.tmp/     in-flight pull staging (renamed to data/)
//
// A per-digest mutex serializes pull/link/GC for a given entry. Live
// references are tracked via st_nlink: nlink == 1 means GC may reclaim.
type ModelStore struct {
	rootDir string
	keyMu   kmutex.KeyedLocker
}

func NewModelStore(rootDir string) *ModelStore {
	return &ModelStore{
		rootDir: rootDir,
		keyMu:   kmutex.New(),
	}
}

// splitDigest validates digest and returns its algo and hex components.
func splitDigest(digest string) (algo, hex string, err error) {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || algo == "" || hex == "" {
		return "", "", errors.Errorf("model store: invalid digest %q", digest)
	}
	if strings.ContainsAny(algo, "/") || strings.ContainsAny(hex, "/") {
		return "", "", errors.Errorf("model store: invalid digest %q", digest)
	}
	return algo, hex, nil
}

func (s *ModelStore) storeDir(digest string) (string, error) {
	algo, hex, err := splitDigest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.rootDir, "store", algo, hex), nil
}

func (s *ModelStore) dataDir(storeDir string) string {
	return filepath.Join(storeDir, "data")
}

func (s *ModelStore) tmpDir(storeDir string) string {
	return filepath.Join(storeDir, "data.tmp")
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Materialize ensures the cache entry for digest is populated and
// hardlinks the cached payload into dstDir. Concurrent callers for the
// same digest are serialized; the first performs the pull, the rest reuse
// the cached data. pullFn is invoked only when data/ is missing.
func (s *ModelStore) Materialize(
	ctx context.Context,
	digest, dstDir string,
	pullFn func(ctx context.Context, dst string) error,
) error {
	storeDir, err := s.storeDir(digest)
	if err != nil {
		return err
	}
	if dstDir == "" {
		return errors.New("model store: empty dst dir")
	}

	if err := s.keyMu.Lock(ctx, digest); err != nil {
		return errors.Wrapf(err, "lock model store digest: %s", digest)
	}
	defer s.keyMu.Unlock(digest)

	dataDir := s.dataDir(storeDir)
	if !dirExists(dataDir) {
		if err := s.pullLocked(ctx, storeDir, pullFn); err != nil {
			return err
		}
	}
	if err := linkTree(dataDir, dstDir); err != nil {
		return errors.Wrapf(err, "link cached data into %s", dstDir)
	}
	return nil
}

// pullLocked pulls into a staging dir then renames it to data/. Must be
// called with keyMu held.
func (s *ModelStore) pullLocked(
	ctx context.Context,
	storeDir string,
	pullFn func(ctx context.Context, dst string) error,
) error {
	dataDir := s.dataDir(storeDir)
	tmpDir := s.tmpDir(storeDir)

	// Clean up any leftover from a previous crashed attempt.
	if err := fsRemoveAll(tmpDir); err != nil {
		return errors.Wrapf(err, "cleanup staging dir: %s", tmpDir)
	}
	if err := fsMkdirAll(tmpDir, 0755); err != nil {
		return errors.Wrapf(err, "mkdir staging dir: %s", tmpDir)
	}

	if err := pullFn(ctx, tmpDir); err != nil {
		_ = fsRemoveAll(tmpDir)
		return err
	}

	if err := fsRename(tmpDir, dataDir); err != nil {
		_ = fsRemoveAll(tmpDir)
		return errors.Wrapf(err, "rename %s -> %s", tmpDir, dataDir)
	}
	return nil
}

// MaybeGC removes the cache entry for digest if no live volumes still
// hardlink its files (st_nlink == 1 on the first regular file under
// data/). Runs under keyMu to race-free against Materialize.
func (s *ModelStore) MaybeGC(ctx context.Context, digest string) error {
	storeDir, err := s.storeDir(digest)
	if err != nil {
		return err
	}

	if err := s.keyMu.Lock(ctx, digest); err != nil {
		return errors.Wrapf(err, "lock model store digest: %s", digest)
	}
	defer s.keyMu.Unlock(digest)

	dataDir := s.dataDir(storeDir)

	if !dirExists(dataDir) {
		// Already gone; drop any leftover staging dir.
		if err := fsRemoveAll(storeDir); err != nil {
			return errors.Wrapf(err, "remove leftover store dir: %s", storeDir)
		}
		return nil
	}

	live, err := hasLiveLinks(dataDir)
	if err != nil {
		return errors.Wrapf(err, "inspect cached payload: %s", dataDir)
	}
	if live {
		return nil
	}
	if err := fsRemoveAll(storeDir); err != nil {
		return errors.Wrapf(err, "remove store dir: %s", storeDir)
	}
	return nil
}

// hasLiveLinks reports whether any regular file under dataDir has
// nlink > 1.
func hasLiveLinks(dataDir string) (bool, error) {
	live := false
	stop := errors.New("stop")
	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			// Unsupported FS: be conservative and report live.
			live = true
			return stop
		}
		if st.Nlink > 1 {
			live = true
			return stop
		}
		return nil
	})
	if errors.Is(err, stop) {
		return live, nil
	}
	return live, err
}

// linkTree mirrors the directory tree at src into dst, hardlinking regular
// files and recreating symlinks. dst is created if missing.
func linkTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return errors.Wrapf(err, "create dst dir: %s", dst)
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return errors.Wrapf(err, "rel path: %s", path)
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return errors.Wrapf(err, "stat dir: %s", path)
			}
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return errors.Wrapf(err, "mkdir: %s", target)
			}
			return nil
		case d.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return errors.Wrapf(err, "readlink: %s", path)
			}
			if err := os.Symlink(link, target); err != nil {
				return errors.Wrapf(err, "symlink %s -> %s", target, link)
			}
			return nil
		default:
			if err := os.Link(path, target); err != nil {
				return errors.Wrapf(err, "hardlink %s -> %s", target, path)
			}
			return nil
		}
	})
}
