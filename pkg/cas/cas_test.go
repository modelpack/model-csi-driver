package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func writeFile(t *testing.T, p string, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
}

// ─── NewStore ────────────────────────────────────────────────────────────────

func TestNewStore_Validation(t *testing.T) {
	_, err := NewStore("")
	require.Error(t, err)
}

func TestNewStore_CreatesDirsAndCleansTmp(t *testing.T) {
	root := t.TempDir()
	// Pre-seed stale tmp files; both naming variants should be cleaned up.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o755))
	writeFile(t, filepath.Join(root, "blobs", "sha256", "abc.tmp.deadbeef"), "x")
	writeFile(t, filepath.Join(root, "blobs", "sha256", "abc.tmp"), "x")

	s, err := NewStore(root)
	require.NoError(t, err)
	require.Equal(t, root, s.Root())

	for _, name := range []string{"abc.tmp.deadbeef", "abc.tmp"} {
		_, err := os.Stat(filepath.Join(root, "blobs", "sha256", name))
		require.True(t, os.IsNotExist(err), name)
	}
}

func TestNewStore_MkdirAllFails(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "as-file")
	writeFile(t, root, "x")
	_, err := NewStore(root)
	require.Error(t, err)
}

// ─── encode/decode helpers and OwnerKey ──────────────────────────────────────

func TestEncodeDecodeDigest(t *testing.T) {
	require.Equal(t, "abc", encodeDigest("sha256:abc"))
	require.Equal(t, "abc", encodeDigest("abc"))
	require.Equal(t, "sha256:abc", decodeDigestFromRefsName("abc.refs"))
	require.Equal(t, "", decodeDigestFromRefsName(".refs"))
	require.Equal(t, "", decodeDigestFromRefsName("abc"))
}

func TestOwnerKey(t *testing.T) {
	require.Equal(t, "v1__m1", OwnerKey("v1", "m1"))
	require.Equal(t, "v1", OwnerKey("v1", ""))
}

// ─── EnsureLink + Import happy path ──────────────────────────────────────────

func TestEnsureLink_MissThenImport_HitOnSecondOwner(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Owner1: cache miss, no skip. Caller "downloads" by writing a file at dst.
	dst1 := filepath.Join(t.TempDir(), "vol1", "weights/model.bin")
	hit, err := s.EnsureLink(context.Background(), "owner1", "sha256:abc", dst1)
	require.NoError(t, err)
	require.False(t, hit)

	writeFile(t, dst1, "BLOB-CONTENT")
	require.NoError(t, s.Import(context.Background(), "sha256:abc", dst1))

	require.True(t, s.Has("sha256:abc"))
	count, err := s.RefCount("sha256:abc")
	require.NoError(t, err)
	require.Equal(t, 1, count)

	// dst1 should now be a hardlink to the canonical blob.
	stA, _ := os.Stat(dst1)
	stB, _ := os.Stat(s.blobPath("sha256:abc"))
	require.True(t, os.SameFile(stA, stB))

	// Owner2: cache hit, file is hardlinked into dst2 directly.
	dst2 := filepath.Join(t.TempDir(), "vol2", "weights/model.bin")
	hit, err = s.EnsureLink(context.Background(), "owner2", "sha256:abc", dst2)
	require.NoError(t, err)
	require.True(t, hit)

	stC, _ := os.Stat(dst2)
	require.True(t, os.SameFile(stB, stC))

	count, err = s.RefCount("sha256:abc")
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestEnsureLink_HitWithoutDst(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Seed the blob via Import.
	dst := filepath.Join(t.TempDir(), "f.bin")
	hit, err := s.EnsureLink(context.Background(), "owner1", "sha256:abc", dst)
	require.NoError(t, err)
	require.False(t, hit)
	writeFile(t, dst, "x")
	require.NoError(t, s.Import(context.Background(), "sha256:abc", dst))

	// Hit path with empty dst is a pure ref-claim.
	hit, err = s.EnsureLink(context.Background(), "owner2", "sha256:abc", "")
	require.NoError(t, err)
	require.True(t, hit)

	count, _ := s.RefCount("sha256:abc")
	require.Equal(t, 2, count)
}

func TestEnsureLink_Validation(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	_, err = s.EnsureLink(context.Background(), "owner", "", "x")
	require.Error(t, err)
	_, err = s.EnsureLink(context.Background(), "", "sha256:abc", "x")
	require.Error(t, err)
}

func TestEnsureLink_HitButLinkFails_StillReportsHit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod ignored for root")
	}
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	dst := filepath.Join(t.TempDir(), "f.bin")
	_, err = s.EnsureLink(context.Background(), "owner1", "sha256:abc", dst)
	require.NoError(t, err)
	writeFile(t, dst, "x")
	require.NoError(t, s.Import(context.Background(), "sha256:abc", dst))

	// Make a destination whose parent dir is read-only.
	parent := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.MkdirAll(parent, 0o500))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	hit, err := s.EnsureLink(context.Background(), "owner2", "sha256:abc", filepath.Join(parent, "child", "f.bin"))
	require.True(t, hit)
	require.Error(t, err)
}

// ─── Concurrency ─────────────────────────────────────────────────────────────

// Concurrent EnsureLink callers for the same missing digest must elect
// exactly one leader (hit=false). Followers block until the leader's Import
// completes, then observe hit=true. All callers end up holding a reference.
func TestEnsureLink_Concurrent_RefsAccountedFor(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	var hits, misses int32
	var leaderDst string
	var leaderOnce sync.Once
	for i := 0; i < N; i++ {
		owner := fmt.Sprintf("owner-%d", i)
		dst := filepath.Join(t.TempDir(), "f.bin")
		go func() {
			defer wg.Done()
			hit, err := s.EnsureLink(context.Background(), owner, "sha256:abc", dst)
			require.NoError(t, err)
			if hit {
				atomic.AddInt32(&hits, 1)
				return
			}
			atomic.AddInt32(&misses, 1)
			leaderOnce.Do(func() {
				leaderDst = dst
				writeFile(t, dst, "BLOB")
				require.NoError(t, s.Import(context.Background(), "sha256:abc", dst))
			})
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), atomic.LoadInt32(&misses), "exactly one leader expected")
	require.Equal(t, int32(N-1), atomic.LoadInt32(&hits))

	count, err := s.RefCount("sha256:abc")
	require.NoError(t, err)
	require.Equal(t, N, count)
	require.NotEmpty(t, leaderDst)
}

// AbortDownload wakes followers so one of them is elected as the new leader.
func TestEnsureLink_LeaderAborts_FollowerBecomesLeader(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	hit, err := s.EnsureLink(context.Background(), "leader", "sha256:abc", "")
	require.NoError(t, err)
	require.False(t, hit)

	followerHit := make(chan bool, 1)
	go func() {
		hit, err := s.EnsureLink(context.Background(), "follower", "sha256:abc", "")
		require.NoError(t, err)
		followerHit <- hit
	}()

	// Give the follower a moment to enter the wait.
	time.Sleep(50 * time.Millisecond)
	s.AbortDownload("sha256:abc")

	select {
	case hit := <-followerHit:
		require.False(t, hit, "follower should be elected as new leader")
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not return after AbortDownload")
	}
}

// A canceled follower must not strand a reference behind.
func TestEnsureLink_FollowerCanceled_RefDropped(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	hit, err := s.EnsureLink(context.Background(), "leader", "sha256:abc", "")
	require.NoError(t, err)
	require.False(t, hit)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, err := s.EnsureLink(ctx, "follower", "sha256:abc", "")
		require.ErrorIs(t, err, context.Canceled)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	// Only the leader's ref should remain.
	n, err := s.RefCount("sha256:abc")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// Two importers for the same digest: the second one should fall back to
// linking against the canonical blob.
func TestImport_RaceLoser_FallsBackToLink(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Pre-create the canonical blob to simulate a winner.
	canonical := s.blobPath("sha256:abc")
	require.NoError(t, os.MkdirAll(filepath.Dir(canonical), 0o755))
	writeFile(t, canonical, "WINNER")

	src := filepath.Join(t.TempDir(), "extract", "f.bin")
	writeFile(t, src, "LOSER")

	require.NoError(t, s.Import(context.Background(), "sha256:abc", src))

	// src should now be a hardlink to the canonical (winner) content.
	body, _ := os.ReadFile(src)
	require.Equal(t, "WINNER", string(body))
}

func TestImport_Validation(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	require.Error(t, s.Import(context.Background(), "", "/x"))
	require.Error(t, s.Import(context.Background(), "sha256:abc", "/does/not/exist"))
}

// Concurrent EnsureLink + ReleaseLayer on the same digest must not strand
// any callers in an inconsistent state. The CAS guarantees that once an
// owner observes a hit (blob present + ref held), the blob will not be
// purged by another owner's release. It does NOT guarantee that every
// concurrent EnsureLink observes the seeded blob: if the seed owner's
// release wins the per-digest lock before another acquirer, that acquirer
// will be elected as the new leader and is expected to re-import.
func TestConcurrent_EnsureLink_Release_NoOrphan(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Seed the blob via Import.
	seedDst := filepath.Join(t.TempDir(), "seed.bin")
	_, err = s.EnsureLink(context.Background(), "ownerSeed", "sha256:abc", seedDst)
	require.NoError(t, err)
	writeFile(t, seedDst, "BLOB")
	require.NoError(t, s.Import(context.Background(), "sha256:abc", seedDst))

	const N = 16
	var wg sync.WaitGroup
	wg.Add(2 * N)
	for i := 0; i < N; i++ {
		owner := fmt.Sprintf("owner-%d", i)
		dst := filepath.Join(t.TempDir(), owner, "f.bin")
		go func() {
			defer wg.Done()
			hit, err := s.EnsureLink(context.Background(), owner, "sha256:abc", dst)
			require.NoError(t, err)
			if !hit {
				// Honor the leader contract: import so any followers can
				// proceed instead of blocking forever.
				writeFile(t, dst, "BLOB")
				require.NoError(t, s.Import(context.Background(), "sha256:abc", dst))
			}
		}()
		go func() {
			defer wg.Done()
			_ = s.ReleaseLayer(context.Background(), "sha256:abc", "ownerSeed")
		}()
	}
	wg.Wait()

	// Every owner now holds a ref pointing to a present blob.
	require.True(t, s.Has("sha256:abc"))
	for i := 0; i < N; i++ {
		owner := fmt.Sprintf("owner-%d", i)
		require.NoError(t, s.ReleaseLayer(context.Background(), "sha256:abc", owner))
	}

	require.False(t, s.Has("sha256:abc"))
}

// ─── ReleaseLayer / ReleaseAll ───────────────────────────────────────────────

func TestReleaseLayer_Validation(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	require.Error(t, s.ReleaseLayer(context.Background(), "", "owner"))
	require.Error(t, s.ReleaseLayer(context.Background(), "sha256:abc", ""))
}

func TestReleaseLayer_Idempotent(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	// Unknown digest: noop.
	require.NoError(t, s.ReleaseLayer(context.Background(), "sha256:unknown", "owner"))

	// Release twice in a row.
	dst := filepath.Join(t.TempDir(), "f.bin")
	_, err = s.EnsureLink(context.Background(), "owner", "sha256:abc", dst)
	require.NoError(t, err)
	writeFile(t, dst, "x")
	require.NoError(t, s.Import(context.Background(), "sha256:abc", dst))

	require.NoError(t, s.ReleaseLayer(context.Background(), "sha256:abc", "owner"))
	require.NoError(t, s.ReleaseLayer(context.Background(), "sha256:abc", "owner"))
	require.False(t, s.Has("sha256:abc"))
}

func TestReleaseAll(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	for _, dg := range []string{"sha256:a", "sha256:b"} {
		dst := filepath.Join(t.TempDir(), "f.bin")
		_, err = s.EnsureLink(context.Background(), "ownerX", dg, dst)
		require.NoError(t, err)
		writeFile(t, dst, dg)
		require.NoError(t, s.Import(context.Background(), dg, dst))
	}
	// ownerY also holds b.
	_, err = s.EnsureLink(context.Background(), "ownerY", "sha256:b", "")
	require.NoError(t, err)

	require.NoError(t, s.ReleaseAll(context.Background(), "ownerX"))
	require.False(t, s.Has("sha256:a"))
	require.True(t, s.Has("sha256:b"))

	require.NoError(t, s.ReleaseAll(context.Background(), "ownerY"))
	require.False(t, s.Has("sha256:b"))
}

func TestReleaseAll_Validation(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	require.Error(t, s.ReleaseAll(context.Background(), ""))
}

func TestReleaseAll_NoBlobsDir(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(root, "blobs")))
	require.NoError(t, s.ReleaseAll(context.Background(), "owner"))
}

// ReleaseAll iterates only entries that look like *.refs; non-.refs files
// (e.g. blobs themselves, accidental garbage) are ignored without error.
func TestReleaseAll_SkipsNonRefsEntries(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(blobsRoot(root), "nope"), 0o755))
	writeFile(t, filepath.Join(blobsRoot(root), "strayfile"), "x")

	require.NoError(t, s.ReleaseAll(context.Background(), "owner"))
}

func TestReleaseAll_InnerErrorPropagates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod is ignored for root")
	}
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	dst := filepath.Join(t.TempDir(), "f.bin")
	_, err = s.EnsureLink(context.Background(), "owner", "sha256:abc", dst)
	require.NoError(t, err)
	writeFile(t, dst, "x")
	require.NoError(t, s.Import(context.Background(), "sha256:abc", dst))

	require.NoError(t, os.Chmod(s.refsDir("sha256:abc"), 0o500))
	t.Cleanup(func() { _ = os.Chmod(s.refsDir("sha256:abc"), 0o755) })

	require.Error(t, s.ReleaseAll(context.Background(), "owner"))
}

// ─── Misc edge cases ─────────────────────────────────────────────────────────

func TestRefCount_Missing(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	c, err := s.RefCount("sha256:none")
	require.NoError(t, err)
	require.Equal(t, 0, c)
}

func TestRefCount_Unreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod ignored for root")
	}
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	dst := filepath.Join(t.TempDir(), "f.bin")
	_, err = s.EnsureLink(context.Background(), "owner", "sha256:abc", dst)
	require.NoError(t, err)
	writeFile(t, dst, "x")
	require.NoError(t, s.Import(context.Background(), "sha256:abc", dst))

	require.NoError(t, os.Chmod(s.refsDir("sha256:abc"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(s.refsDir("sha256:abc"), 0o755) })

	_, err = s.RefCount("sha256:abc")
	require.Error(t, err)
}

func TestEnsureLink_CanceledContext(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.EnsureLink(ctx, "owner", "sha256:abc", "")
	require.Error(t, err)
}

func TestImport_CanceledContext(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	src := filepath.Join(t.TempDir(), "f.bin")
	writeFile(t, src, "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, s.Import(ctx, "sha256:abc", src))
}

func TestReleaseLayer_CanceledContext(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, s.ReleaseLayer(ctx, "sha256:abc", "owner"))
}

func TestEnsureLink_AddRefMkdirFails(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	// Pre-create refs dir as a regular file so MkdirAll inside addRefLocked fails.
	writeFile(t, s.refsDir("sha256:abc"), "x")
	_, err = s.EnsureLink(context.Background(), "owner", "sha256:abc", "")
	require.Error(t, err)
}

func TestEnsureLink_AddRefOpenFileFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod ignored for root")
	}
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(s.refsDir("sha256:abc"), 0o500))
	t.Cleanup(func() { _ = os.Chmod(s.refsDir("sha256:abc"), 0o755) })
	_, err = s.EnsureLink(context.Background(), "owner", "sha256:abc", "")
	require.Error(t, err)
}

func TestCleanupStaleTmp_NotExist(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(blobsRoot(root)))
	require.NoError(t, s.cleanupStaleTmp())
}

func TestCleanupStaleTmp_ReadDirError(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Replace blobs/sha256 with a regular file; ReadDir then errors out.
	require.NoError(t, os.RemoveAll(blobsRoot(root)))
	writeFile(t, blobsRoot(root), "x")
	require.Error(t, s.cleanupStaleTmp())
}

func TestImport_RenameSrcFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod ignored for root")
	}
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	srcDir := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	src := filepath.Join(srcDir, "f.bin")
	writeFile(t, src, "x")
	// Make srcDir read-only so the rename(src, tmp) call fails.
	require.NoError(t, os.Chmod(srcDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(srcDir, 0o755) })

	require.Error(t, s.Import(context.Background(), "sha256:abc", src))
}

func TestFileExistsHelpers(t *testing.T) {
	dir := t.TempDir()
	require.False(t, fileExists(filepath.Join(dir, "x")))
	require.False(t, fileExists(dir))
	writeFile(t, filepath.Join(dir, "x"), "y")
	require.True(t, fileExists(filepath.Join(dir, "x")))

	require.True(t, dirExists(dir))
	require.False(t, dirExists(filepath.Join(dir, "missing")))
	require.False(t, dirExists(filepath.Join(dir, "x")))
}

func TestIsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	empty, err := isEmptyDir(dir)
	require.NoError(t, err)
	require.True(t, empty)

	writeFile(t, filepath.Join(dir, "f"), "x")
	empty, err = isEmptyDir(dir)
	require.NoError(t, err)
	require.False(t, empty)

	_, err = isEmptyDir(filepath.Join(dir, "missing"))
	require.Error(t, err)
}

// keep the package's "errors" import used at least once even if no test
// currently constructs an error.
var _ = errors.New

// ─── Edge cases for coverage ─────────────────────────────────────────────────

// NewStore should propagate cleanupStaleTmp errors (e.g. blobs/sha256 is a
// regular file rather than a directory).
func TestNewStore_CleanupTmpFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod ignored for root")
	}
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(blobsRoot(root), 0o755))
	// Make blobs/sha256 unreadable so the ReadDir inside cleanupStaleTmp
	// (called from NewStore) returns a non-NotExist error.
	require.NoError(t, os.Chmod(blobsRoot(root), 0o000))
	t.Cleanup(func() { _ = os.Chmod(blobsRoot(root), 0o755) })

	_, err := NewStore(root)
	require.Error(t, err)
}

// Import: rename(tmp, final) fails because final's parent is read-only,
// but the source is restored so the caller still has its extract.
func TestImport_PublishFails_RestoresSrc(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod ignored for root")
	}
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	src := filepath.Join(t.TempDir(), "f.bin")
	writeFile(t, src, "BLOB")

	// Make the blobs dir read-only so the publish rename fails.
	require.NoError(t, os.Chmod(blobsRoot(root), 0o500))
	t.Cleanup(func() { _ = os.Chmod(blobsRoot(root), 0o755) })

	err = s.Import(context.Background(), "sha256:abc", src)
	require.Error(t, err)
}

// ReleaseLayer: when refsDir disappeared (e.g. due to a concurrent cleanup),
// the call should still drop the blob and return nil.
func TestReleaseLayer_RefsDirMissing_RemovesBlob(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Set up a canonical blob with no refs dir.
	writeFile(t, s.blobPath("sha256:abc"), "BLOB")

	require.NoError(t, s.ReleaseLayer(context.Background(), "sha256:abc", "owner"))
	require.False(t, s.Has("sha256:abc"))
}

// ReleaseLayer: refsDir stat error path other than NotExist (refs path is a
// regular file, so isEmptyDir errors).
func TestReleaseLayer_RefsDirStatError(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Make the refs path a regular file so isEmptyDir returns a non-NotExist error.
	writeFile(t, s.refsDir("sha256:abc"), "x")
	require.Error(t, s.ReleaseLayer(context.Background(), "sha256:abc", "owner"))
}

// ReleaseAll: blobs dir is unreadable -> error propagated.
func TestReleaseAll_BlobsDirUnreadable(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Replace blobs/sha256 with a regular file so ReadDir errors.
	require.NoError(t, os.RemoveAll(blobsRoot(root)))
	writeFile(t, blobsRoot(root), "x")
	require.Error(t, s.ReleaseAll(context.Background(), "owner"))
}

// ReleaseAll: refs dir exists for digest but the owner has no entry in it
// (Stat fails with NotExist) -> the digest is silently skipped.
func TestReleaseAll_OwnerMissingFromRefsDir_Skips(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root)
	require.NoError(t, err)

	// Seed a refs dir with another owner.
	hit, err := s.EnsureLink(context.Background(), "otherOwner", "sha256:abc", "")
	require.NoError(t, err)
	require.False(t, hit)

	// ReleaseAll for an owner with no ref must succeed and not touch the dir.
	require.NoError(t, s.ReleaseAll(context.Background(), "missing"))
	n, err := s.RefCount("sha256:abc")
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
