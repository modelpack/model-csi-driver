package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

func newTestCache(t *testing.T) (*LayerCache, string) {
	t.Helper()
	tmp := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmp}
	cfg := config.NewWithRaw(rawCfg)
	require.NoError(t, os.MkdirAll(cfg.Get().GetVolumesDir(), 0755))
	return NewLayerCache(cfg), tmp
}

func writeFile(t *testing.T, p string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
	require.NoError(t, os.WriteFile(p, data, 0644))
}

func TestLayerCache_Acquire_NoEntry_ReturnsPull(t *testing.T) {
	c, tmp := newTestCache(t)
	target := filepath.Join(tmp, "volumes/pvc-1/model/a.bin")

	action, err := c.Acquire(context.Background(), digest.FromString("a"), target)
	require.NoError(t, err)
	require.Equal(t, ActionPull, action)

	c.mu.Lock()
	e := c.items[digest.FromString("a")]
	c.mu.Unlock()
	require.NotNil(t, e)
	e.mu.Lock()
	require.Equal(t, statePulling, e.state)
	e.mu.Unlock()
}

func TestLayerCache_Acquire_EmptyDigestOrTarget(t *testing.T) {
	c, _ := newTestCache(t)
	a, err := c.Acquire(context.Background(), "", "/x")
	require.NoError(t, err)
	require.Equal(t, ActionPull, a)
	a, err = c.Acquire(context.Background(), digest.FromString("a"), "")
	require.NoError(t, err)
	require.Equal(t, ActionPull, a)
}

func TestLayerCache_PublishAndHardlinkHit(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("layer1")

	src := filepath.Join(tmp, "volumes/pvc-a/model/file.bin")
	writeFile(t, src, []byte("hello"))

	// Owner publishes.
	_, _ = c.Acquire(context.Background(), d, src)
	c.Publish(d, src)
	c.FlushPersist()

	// Persisted layers.json contains the path.
	data, err := os.ReadFile(filepath.Join(tmp, "volumes/pvc-a/layers.json"))
	require.NoError(t, err)
	var f layersFile
	require.NoError(t, json.Unmarshal(data, &f))
	require.Len(t, f.Items, 1)
	require.Equal(t, src, f.Items[0].Path)

	// New target on same digest hits and gets hardlinked.
	dst := filepath.Join(tmp, "volumes/pvc-b/model/file.bin")
	action, err := c.Acquire(context.Background(), d, dst)
	require.NoError(t, err)
	require.Equal(t, ActionHit, action)

	srcStat, err := os.Stat(src)
	require.NoError(t, err)
	dstStat, err := os.Stat(dst)
	require.NoError(t, err)
	require.True(t, os.SameFile(srcStat, dstStat))
}

func TestLayerCache_AcquireSameTargetTwice_NoOp(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("same")
	target := filepath.Join(tmp, "volumes/pvc-a/model/x.bin")

	_, _ = c.Acquire(context.Background(), d, target)
	writeFile(t, target, []byte("x"))
	c.Publish(d, target)

	action, err := c.Acquire(context.Background(), d, target)
	require.NoError(t, err)
	require.Equal(t, ActionHit, action)
}

func TestLayerCache_StalePathDropped(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("stale")
	gone := filepath.Join(tmp, "volumes/pvc-x/model/gone.bin")
	writeFile(t, gone, []byte("g"))

	_, _ = c.Acquire(context.Background(), d, gone)
	c.Publish(d, gone)

	require.NoError(t, os.Remove(gone))

	target := filepath.Join(tmp, "volumes/pvc-y/model/x.bin")
	action, err := c.Acquire(context.Background(), d, target)
	require.NoError(t, err)
	require.Equal(t, ActionPull, action)

	c.mu.Lock()
	e := c.items[d]
	c.mu.Unlock()
	e.mu.Lock()
	require.Empty(t, e.paths)
	e.mu.Unlock()
}

func TestLayerCache_Wait_ThenHit(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("wait-hit")
	src := filepath.Join(tmp, "volumes/pvc-a/model/w.bin")

	// A acquires as owner.
	a, err := c.Acquire(context.Background(), d, src)
	require.NoError(t, err)
	require.Equal(t, ActionPull, a)

	// B starts and should block until A publishes.
	type result struct {
		action Action
		err    error
	}
	dst := filepath.Join(tmp, "volumes/pvc-b/model/w.bin")
	resCh := make(chan result, 1)
	go func() {
		action, err := c.Acquire(context.Background(), d, dst)
		resCh <- result{action, err}
	}()

	time.Sleep(50 * time.Millisecond)
	select {
	case <-resCh:
		t.Fatal("B should be waiting")
	default:
	}

	writeFile(t, src, []byte("w"))
	c.Publish(d, src)

	select {
	case r := <-resCh:
		require.NoError(t, r.err)
		require.Equal(t, ActionHit, r.action)
	case <-time.After(2 * time.Second):
		t.Fatal("B did not wake up")
	}
}

func TestLayerCache_Wait_ThenFail_Fallback(t *testing.T) {
	c, _ := newTestCache(t)
	d := digest.FromString("wait-fail")

	// A becomes owner.
	a, err := c.Acquire(context.Background(), d, "/tmp/never/a.bin")
	require.NoError(t, err)
	require.Equal(t, ActionPull, a)

	resCh := make(chan Action, 1)
	go func() {
		action, _ := c.Acquire(context.Background(), d, "/tmp/never/b.bin")
		resCh <- action
	}()

	time.Sleep(50 * time.Millisecond)
	c.Fail(d)

	select {
	case action := <-resCh:
		require.Equal(t, ActionPull, action)
	case <-time.After(2 * time.Second):
		t.Fatal("B did not wake up")
	}

	// Entry should be in pulling state again, owned by B.
	c.mu.Lock()
	e := c.items[d]
	c.mu.Unlock()
	e.mu.Lock()
	require.Equal(t, statePulling, e.state)
	e.mu.Unlock()
}

func TestLayerCache_Wait_CtxCancel(t *testing.T) {
	c, _ := newTestCache(t)
	d := digest.FromString("wait-ctx")

	_, err := c.Acquire(context.Background(), d, "/tmp/x/a.bin")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	resCh := make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, d, "/tmp/x/b.bin")
		resCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-resCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("B did not return after ctx cancel")
	}
}

func TestLayerCache_Acquire_CtxAlreadyDone(t *testing.T) {
	c, _ := newTestCache(t)
	d := digest.FromString("ctx-pre")
	_, err := c.Acquire(context.Background(), d, "/tmp/y/a.bin")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Acquire(ctx, d, "/tmp/y/b.bin")
	require.ErrorIs(t, err, context.Canceled)
}

func TestLayerCache_Acquire_ContextCanceledBeforeSemaphore(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("ctx-sem")
	target := filepath.Join(tmp, "volumes/pvc-a/model/x.bin")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	action, err := c.Acquire(ctx, d, target)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ActionPull, action)
}

func TestLayerCache_Acquire_RechecksHardlinkAfterSemaphoreWait(t *testing.T) {
	tmp := t.TempDir()
	rawCfg := &config.RawConfig{
		ServiceName: "test",
		RootDir:     tmp,
		PullConfig:  config.PullConfig{NodeLayerConcurrency: 1},
	}
	cfg := config.NewWithRaw(rawCfg)
	require.NoError(t, os.MkdirAll(cfg.Get().GetVolumesDir(), 0755))
	c := NewLayerCache(cfg)
	d := digest.FromString("sem-recheck-hit")

	require.NoError(t, c.sem.Acquire(context.Background(), 1))
	src := filepath.Join(tmp, "volumes/pvc-a/model/layer.bin")
	dst := filepath.Join(tmp, "volumes/pvc-b/model/layer.bin")

	type result struct {
		action Action
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		action, err := c.Acquire(context.Background(), d, dst)
		resCh <- result{action: action, err: err}
	}()

	// Give the goroutine time to block on the exhausted semaphore, then make a
	// usable source path appear before the semaphore is released.
	time.Sleep(50 * time.Millisecond)
	writeFile(t, src, []byte("layer"))
	e := c.getOrCreateEntry(d)
	e.mu.Lock()
	e.paths = []string{src}
	e.state = stateDone
	e.mu.Unlock()

	c.sem.Release(1)

	select {
	case r := <-resCh:
		require.NoError(t, r.err)
		require.Equal(t, ActionHit, r.action)
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not resume after semaphore release")
	}
}

func TestLayerCache_Acquire_WaitsWhenAnotherOwnerWinsSemaphoreRace(t *testing.T) {
	tmp := t.TempDir()
	rawCfg := &config.RawConfig{
		ServiceName: "test",
		RootDir:     tmp,
		PullConfig:  config.PullConfig{NodeLayerConcurrency: 1},
	}
	cfg := config.NewWithRaw(rawCfg)
	require.NoError(t, os.MkdirAll(cfg.Get().GetVolumesDir(), 0755))
	c := NewLayerCache(cfg)
	d := digest.FromString("sem-recheck-pulling")
	target := filepath.Join(tmp, "volumes/pvc-a/model/layer.bin")

	require.NoError(t, c.sem.Acquire(context.Background(), 1))
	type result struct {
		action Action
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		action, err := c.Acquire(context.Background(), d, target)
		resCh <- result{action: action, err: err}
	}()

	// While the goroutine is waiting for semaphore capacity, simulate another
	// puller becoming the owner for the same digest.
	time.Sleep(50 * time.Millisecond)
	e := c.getOrCreateEntry(d)
	e.mu.Lock()
	e.state = statePulling
	e.mu.Unlock()
	c.sem.Release(1)

	require.Eventually(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.waiters) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Wake the waiter without releasing the semaphore; the waiter should loop,
	// take ownership itself, and return ActionPull.
	e.mu.Lock()
	e.state = stateIdle
	e.notifyWaiters()
	e.mu.Unlock()

	select {
	case r := <-resCh:
		require.NoError(t, r.err)
		require.Equal(t, ActionPull, r.action)
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not resume after waiter notification")
	}
	c.Fail(d)
}

func TestLayerCache_OnVolumeRemoved(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("rm")
	volA := filepath.Join(tmp, "volumes/pvc-a")
	srcA := filepath.Join(volA, "model/r.bin")
	writeFile(t, srcA, []byte("r"))
	_, _ = c.Acquire(context.Background(), d, srcA)
	c.Publish(d, srcA)

	dstB := filepath.Join(tmp, "volumes/pvc-b/model/r.bin")
	_, err := c.Acquire(context.Background(), d, dstB)
	require.NoError(t, err)

	// Remove A.
	c.OnVolumeRemoved(volA)
	c.mu.Lock()
	e := c.items[d]
	owned := c.perVolume[volA]
	c.mu.Unlock()
	require.Nil(t, owned)
	e.mu.Lock()
	for _, p := range e.paths {
		require.NotEqual(t, srcA, p)
	}
	e.mu.Unlock()
}

func TestLayerCache_OnVolumeRemoved_EmptyArgs(t *testing.T) {
	c, _ := newTestCache(t)
	c.OnVolumeRemoved("") // must not panic
}

func TestLayerCache_OnVolumeRemoved_LastPathResetsDoneEntry(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("rm-last")
	volA := filepath.Join(tmp, "volumes/pvc-last")
	src := filepath.Join(volA, "model/layer.bin")
	writeFile(t, src, []byte("layer"))

	_, err := c.Acquire(context.Background(), d, src)
	require.NoError(t, err)
	c.Publish(d, src)

	c.OnVolumeRemoved(volA)

	c.mu.Lock()
	e := c.items[d]
	_, stillOwned := c.perVolume[volA]
	c.mu.Unlock()
	require.False(t, stillOwned)
	require.NotNil(t, e)
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Empty(t, e.paths)
	require.Equal(t, stateIdle, e.state)
}

func TestLayerCache_Rebuild_FromDisk(t *testing.T) {
	c, tmp := newTestCache(t)
	dir := filepath.Join(tmp, "volumes/pvc-r")
	good := filepath.Join(dir, "model/good.bin")
	gone := filepath.Join(dir, "model/gone.bin")
	writeFile(t, good, []byte("g"))

	items := map[digest.Digest]string{
		digest.FromString("good"): good,
		digest.FromString("gone"): gone,
	}
	require.NoError(t, writeLayersFile(dir, items))

	require.NoError(t, c.Rebuild(context.Background()))

	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotNil(t, c.items[digest.FromString("good")])
	require.Nil(t, c.items[digest.FromString("gone")])
}

func TestLayerCache_Rebuild_DynamicMounts(t *testing.T) {
	c, tmp := newTestCache(t)
	dyn := filepath.Join(tmp, "volumes/csi-1/models/m1")
	good := filepath.Join(dyn, "model/d.bin")
	writeFile(t, good, []byte("d"))
	require.NoError(t, writeLayersFile(dyn, map[digest.Digest]string{
		digest.FromString("d"): good,
	}))

	require.NoError(t, c.Rebuild(context.Background()))
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotNil(t, c.items[digest.FromString("d")])
	require.NotNil(t, c.perVolume[dyn])
}

func TestLayerCache_Rebuild_MissingDir(t *testing.T) {
	tmp := t.TempDir()
	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: filepath.Join(tmp, "absent")}
	cfg := config.NewWithRaw(rawCfg)
	c := NewLayerCache(cfg)
	require.NoError(t, c.Rebuild(context.Background()))
}

func TestLayerCache_Rebuild_MalformedFileIgnored(t *testing.T) {
	c, tmp := newTestCache(t)
	dir := filepath.Join(tmp, "volumes/pvc-bad")
	writeFile(t, filepath.Join(dir, LayersFileName), []byte("not-json"))
	require.NoError(t, c.Rebuild(context.Background()))
	c.mu.Lock()
	require.Empty(t, c.items)
	c.mu.Unlock()
}

func TestLayerCache_Rebuild_SkipsEmptyDigestOrPathItems(t *testing.T) {
	c, tmp := newTestCache(t)
	dir := filepath.Join(tmp, "volumes/pvc-invalid-items")
	goodDigest := digest.FromString("good-item")
	goodPath := filepath.Join(dir, "model/good.bin")
	writeFile(t, goodPath, []byte("good"))

	f := layersFile{Schema: 1, Items: []layersFileItem{
		{Digest: "", Path: filepath.Join(dir, "model/empty-digest.bin")},
		{Digest: digest.FromString("empty-path"), Path: ""},
		{Digest: goodDigest, Path: goodPath},
	}}
	data, err := json.Marshal(f)
	require.NoError(t, err)
	writeFile(t, filepath.Join(dir, LayersFileName), data)

	require.NoError(t, c.Rebuild(context.Background()))

	c.mu.Lock()
	defer c.mu.Unlock()
	require.Len(t, c.items, 1)
	require.NotNil(t, c.items[goodDigest])
	require.Nil(t, c.items[digest.FromString("empty-path")])
}

func TestLayerCache_VolumeDirFor(t *testing.T) {
	c, tmp := newTestCache(t)
	root := filepath.Join(tmp, "volumes")

	require.Equal(t, filepath.Join(root, "pvc-1"), c.volumeDirFor(filepath.Join(root, "pvc-1/model/a.bin")))
	require.Equal(t, filepath.Join(root, "csi-1/models/m1"), c.volumeDirFor(filepath.Join(root, "csi-1/models/m1/model/a.bin")))
	require.Equal(t, "", c.volumeDirFor("/elsewhere/x"))
}

func TestLayerCache_Concurrent_OneOwnerRestHit(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("conc")

	const N = 8
	var owners atomic.Int32
	var hits atomic.Int32

	wg := sync.WaitGroup{}
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			target := filepath.Join(tmp, "volumes/pvc-c", "model", "f.bin")
			if i > 0 {
				target = filepath.Join(tmp, "volumes/pvc-d-"+string(rune('a'+i)), "model/f.bin")
			}
			action, err := c.Acquire(context.Background(), d, target)
			require.NoError(t, err)
			if action == ActionPull {
				owners.Add(1)
				writeFile(t, target, []byte("f"))
				c.Publish(d, target)
			} else {
				hits.Add(1)
			}
		}()
	}
	wg.Wait()

	require.EqualValues(t, 1, owners.Load(), "exactly one owner expected")
	require.EqualValues(t, N-1, hits.Load(), "rest must hit")
}

func TestWriteLayersFile_TmpRenameVisible(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "vol")
	items := map[digest.Digest]string{digest.FromString("z"): "/path/z"}
	require.NoError(t, writeLayersFile(dir, items))
	_, err := os.Stat(filepath.Join(dir, LayersFileName))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, LayersFileName+".tmp"))
	require.True(t, os.IsNotExist(err))
}

func TestHardlink_OverwritesExistingTarget(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	dst := filepath.Join(tmp, "sub", "dst")
	require.NoError(t, os.WriteFile(src, []byte("s"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0755))
	require.NoError(t, os.WriteFile(dst, []byte("old"), 0644))

	require.NoError(t, hardlink(src, dst))
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "s", string(got))
}

func TestHardlink_MissingSourceFails(t *testing.T) {
	tmp := t.TempDir()
	err := hardlink(filepath.Join(tmp, "missing"), filepath.Join(tmp, "x"))
	require.Error(t, err)
}

func TestUniqueStrings(t *testing.T) {
	require.Equal(t, []string{"a"}, uniqueStrings([]string{"a"}))
	require.Equal(t, []string{"a", "b"}, uniqueStrings([]string{"a", "b", "a", "b"}))
}

func TestLayerCache_Fail_UnknownDigest(t *testing.T) {
	c, _ := newTestCache(t)
	c.Fail(digest.FromString("nope")) // must not panic
	c.Fail("")                        // must not panic
}

func TestLayerCache_Publish_EmptyArgs(t *testing.T) {
	c, _ := newTestCache(t)
	c.Publish("", "/x") // must not panic
	c.Publish(digest.FromString("a"), "")
}

func TestLayerCache_Fail_PreservesPathsWhenSomeRemain(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("mix")
	src := filepath.Join(tmp, "volumes/pvc-a/model/m.bin")
	writeFile(t, src, []byte("m"))
	_, _ = c.Acquire(context.Background(), d, src)
	c.Publish(d, src)

	// Drive a manual transition into stateIdle by removing src and
	// having a new acquire become the owner that subsequently fails.
	require.NoError(t, os.Remove(src))
	_, _ = c.Acquire(context.Background(), d, filepath.Join(tmp, "volumes/pvc-b/model/m.bin"))
	c.Fail(d)

	c.mu.Lock()
	e := c.items[d]
	c.mu.Unlock()
	e.mu.Lock()
	require.Equal(t, stateIdle, e.state)
	e.mu.Unlock()
}

// Ensure error returned by writeLayersFile when dir cannot be created
// surfaces (e.g. when path collides with file).
func TestWriteLayersFile_MkdirError(t *testing.T) {
	tmp := t.TempDir()
	clash := filepath.Join(tmp, "clash")
	require.NoError(t, os.WriteFile(clash, []byte("x"), 0644))
	err := writeLayersFile(clash, nil)
	require.Error(t, err)
}

// Covers tryHardlinkLocked fallback when hardlink fails: source path is
// preserved, no kept target, action stays ActionPull.
func TestLayerCache_Acquire_HardlinkFails_PreservesSource(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("hl-fail")

	src := filepath.Join(tmp, "volumes/pvc-a/model/src.bin")
	writeFile(t, src, []byte("x"))

	// Pre-seed the entry with the source path so tryHardlinkLocked has
	// something to attempt linking from.
	e := c.getOrCreateEntry(d)
	e.mu.Lock()
	e.paths = []string{src}
	e.state = stateDone
	e.mu.Unlock()

	// Make the parent path of the destination a regular file so MkdirAll
	// inside hardlink() fails.
	parentAsFile := filepath.Join(tmp, "parent-file")
	require.NoError(t, os.WriteFile(parentAsFile, []byte("f"), 0644))
	target := filepath.Join(parentAsFile, "dst.bin")

	action, err := c.Acquire(context.Background(), d, target)
	require.NoError(t, err)
	require.Equal(t, ActionPull, action)

	// Source path must be preserved on fallback.
	e.mu.Lock()
	require.Contains(t, e.paths, src)
	require.NotContains(t, e.paths, target)
	e.mu.Unlock()
}

// Covers Fail() when the entry has remaining paths (state stays stateDone).
func TestLayerCache_Fail_KeepsStateDoneWhenPathsRemain(t *testing.T) {
	c, tmp := newTestCache(t)
	d := digest.FromString("fail-keep")
	src := filepath.Join(tmp, "volumes/pvc-a/model/k.bin")
	writeFile(t, src, []byte("k"))

	_, _ = c.Acquire(context.Background(), d, src)
	c.Publish(d, src)

	c.Fail(d)

	c.mu.Lock()
	e := c.items[d]
	c.mu.Unlock()
	e.mu.Lock()
	require.Equal(t, stateDone, e.state)
	require.Contains(t, e.paths, src)
	e.mu.Unlock()
}

// Covers OnVolumeRemoved when c.items[d] is nil (digest is in perVolume but
// not in items). Must not panic.
func TestLayerCache_OnVolumeRemoved_GhostDigest(t *testing.T) {
	c, tmp := newTestCache(t)
	volDir := filepath.Join(tmp, "volumes/pvc-ghost")

	c.mu.Lock()
	c.perVolume[volDir] = map[digest.Digest]string{
		digest.FromString("ghost"): "/x",
	}
	c.mu.Unlock()

	c.OnVolumeRemoved(volDir)

	c.mu.Lock()
	_, exists := c.perVolume[volDir]
	c.mu.Unlock()
	require.False(t, exists)
}

// Covers Rebuild() returning an error when the volumes dir cannot be read.
func TestLayerCache_Rebuild_ReadDirError(t *testing.T) {
	tmp := t.TempDir()
	// Make the volumes dir actually be a regular file so ReadDir returns
	// a non-IsNotExist error.
	require.NoError(t, os.MkdirAll(tmp, 0755))
	volumesPath := filepath.Join(tmp, "volumes")
	require.NoError(t, os.WriteFile(volumesPath, []byte("f"), 0644))

	rawCfg := &config.RawConfig{ServiceName: "test", RootDir: tmp}
	cfg := config.NewWithRaw(rawCfg)
	c := NewLayerCache(cfg)

	err := c.Rebuild(context.Background())
	require.Error(t, err)
}

// Covers Rebuild() skipping a non-directory entry at the volumes root.
func TestLayerCache_Rebuild_SkipsNonDirEntries(t *testing.T) {
	c, tmp := newTestCache(t)
	root := filepath.Join(tmp, "volumes")
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0644))

	require.NoError(t, c.Rebuild(context.Background()))
}

// Covers Rebuild() skipping a non-directory entry inside the dynamic
// `models/` subtree.
func TestLayerCache_Rebuild_SkipsNonDirInsideModels(t *testing.T) {
	c, tmp := newTestCache(t)
	modelsDir := filepath.Join(tmp, "volumes/csi-1/models")
	require.NoError(t, os.MkdirAll(modelsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(modelsDir, "stray"), []byte("x"), 0644))

	require.NoError(t, c.Rebuild(context.Background()))
}

// Covers recordVolumeMapping when volumeDirFor returns "" (target outside
// the volumes root): nothing is persisted.
func TestLayerCache_Publish_OutsideVolumesRoot_NoLayersFile(t *testing.T) {
	c, _ := newTestCache(t)
	extTmp := t.TempDir()
	target := filepath.Join(extTmp, "elsewhere", "x.bin")
	writeFile(t, target, []byte("x"))

	d := digest.FromString("outside")
	c.Publish(d, target)

	// No layers.json next to the target.
	_, err := os.Stat(filepath.Join(filepath.Dir(target), LayersFileName))
	require.True(t, os.IsNotExist(err))

	// Cache still tracks the entry, but it isn't in perVolume.
	c.mu.Lock()
	require.NotNil(t, c.items[d])
	require.Empty(t, c.perVolume)
	c.mu.Unlock()
}

// Covers volumeDirFor when target equals the volumes root: rel == ".".
func TestLayerCache_VolumeDirFor_RootItself(t *testing.T) {
	c, tmp := newTestCache(t)
	root := filepath.Join(tmp, "volumes")
	// rel is ".", parts=["."], not prefixed with "..", and len>0 so it
	// returns filepath.Join(root, ".") == root.
	require.Equal(t, root, c.volumeDirFor(root))
}

// Covers hardlink() returning an error when the destination's parent path
// already exists as a regular file (MkdirAll fails).
func TestHardlink_TargetParentIsFile(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	require.NoError(t, os.WriteFile(src, []byte("s"), 0644))

	parent := filepath.Join(tmp, "parent")
	require.NoError(t, os.WriteFile(parent, []byte("f"), 0644))
	dst := filepath.Join(parent, "dst")

	err := hardlink(src, dst)
	require.Error(t, err)
}

func TestLayerEntry_NotifyWaiters_DropsNotificationWhenChannelFull(t *testing.T) {
	e := newLayerEntry()
	full := make(chan struct{}, 1)
	full <- struct{}{}
	ready := make(chan struct{}, 1)
	e.waiters = []chan struct{}{full, ready}

	e.notifyWaiters()

	require.Empty(t, e.waiters)
	require.Len(t, full, 1)
	select {
	case <-ready:
	default:
		t.Fatal("expected ready waiter to be notified")
	}
}

func TestLayerCache_FlushDirtyVolumes_WriteError(t *testing.T) {
	c, tmp := newTestCache(t)
	clash := filepath.Join(tmp, "vol-file")
	require.NoError(t, os.WriteFile(clash, []byte("not a directory"), 0644))

	c.indexVolumeMapping(clash, digest.FromString("flush-error"), filepath.Join(clash, "layer.bin"))
	c.dirtyMu.Lock()
	c.dirtyVols = map[string]struct{}{clash: {}}
	c.dirtyMu.Unlock()

	c.flushDirtyVolumes()
}

func TestLayerCache_FlushPersist_WriteError(t *testing.T) {
	c, tmp := newTestCache(t)
	clash := filepath.Join(tmp, "persist-file")
	require.NoError(t, os.WriteFile(clash, []byte("not a directory"), 0644))

	c.indexVolumeMapping(clash, digest.FromString("persist-error"), filepath.Join(clash, "layer.bin"))
	c.dirtyMu.Lock()
	c.dirtyVols = map[string]struct{}{clash: {}}
	c.dirtyMu.Unlock()

	c.FlushPersist()
}

func TestWriteLayersFile_WriteTmpError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to read-only directories")
	}
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "readonly")
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.Chmod(dir, 0555))
	defer func() { require.NoError(t, os.Chmod(dir, 0755)) }()

	err := writeLayersFile(dir, map[digest.Digest]string{digest.FromString("tmp-error"): "/path/layer.bin"})
	require.Error(t, err)
}

func TestWriteLayersFile_RenameError(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "rename-error")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, LayersFileName), 0755))

	err := writeLayersFile(dir, map[digest.Digest]string{digest.FromString("rename-error"): "/path/layer.bin"})
	require.Error(t, err)
}

func TestHardlink_RemoveStaleTargetError(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	require.NoError(t, os.WriteFile(src, []byte("s"), 0644))
	dst := filepath.Join(tmp, "dst")
	require.NoError(t, os.MkdirAll(dst, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dst, "child"), []byte("x"), 0644))

	err := hardlink(src, dst)
	require.Error(t, err)
}
