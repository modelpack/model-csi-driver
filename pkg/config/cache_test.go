package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSplitDigest covers the digest parser used to build cache paths.
func TestSplitDigest(t *testing.T) {
	cases := []struct {
		in       string
		wantAlgo string
		wantHex  string
	}{
		{"sha256:abc123", "sha256", "abc123"},
		{"sha512:deadbeef", "sha512", "deadbeef"},
		{"abc123", "sha256", "abc123"}, // no colon → default algo
		{"sha256:", "sha256", ""},      // empty hex still splits
	}
	for _, c := range cases {
		algo, hex := splitDigest(c.in)
		require.Equal(t, c.wantAlgo, algo, "algo for %q", c.in)
		require.Equal(t, c.wantHex, hex, "hex for %q", c.in)
	}
}

// TestGetCacheDirs exercises every cache path helper at once and checks the
// two parallel trees (content/ and refs/) share the same algo/hex suffix for
// any given digest.
func TestGetCacheDirs(t *testing.T) {
	cfg := &RawConfig{RootDir: "/var/lib/model-csi"}

	require.Equal(t, "/var/lib/model-csi/cache", cfg.GetCacheDir())
	require.Equal(t,
		filepath.Join("/var/lib/model-csi", "cache", "content"),
		cfg.GetCacheContentRootDir(),
	)
	require.Equal(t,
		filepath.Join("/var/lib/model-csi", "cache", "refs"),
		cfg.GetCacheRefsRootDir(),
	)

	// Canonical sha256 digest.
	digest := "sha256:69a0c4d9505eb64e2454444baac2f5273c12450942f5a117e83557d161fb1206"
	require.Equal(t,
		filepath.Join("/var/lib/model-csi", "cache", "content", "sha256", "69a0c4d9505eb64e2454444baac2f5273c12450942f5a117e83557d161fb1206"),
		cfg.GetCacheContentDir(digest),
	)
	require.Equal(t,
		filepath.Join("/var/lib/model-csi", "cache", "refs", "sha256", "69a0c4d9505eb64e2454444baac2f5273c12450942f5a117e83557d161fb1206"),
		cfg.GetCacheRefsDir(digest),
	)

	// Non-sha256 algo is respected end to end.
	require.Equal(t,
		filepath.Join("/var/lib/model-csi", "cache", "content", "sha512", "deadbeef"),
		cfg.GetCacheContentDir("sha512:deadbeef"),
	)
	require.Equal(t,
		filepath.Join("/var/lib/model-csi", "cache", "refs", "sha512", "deadbeef"),
		cfg.GetCacheRefsDir("sha512:deadbeef"),
	)

	// Digest without algo prefix falls back to sha256.
	require.Equal(t,
		filepath.Join("/var/lib/model-csi", "cache", "content", "sha256", "abc123"),
		cfg.GetCacheContentDir("abc123"),
	)

	// content/ and refs/ must share the same algo/hex suffix for a digest.
	contentRel, err := filepath.Rel(cfg.GetCacheContentRootDir(), cfg.GetCacheContentDir(digest))
	require.NoError(t, err)
	refsRel, err := filepath.Rel(cfg.GetCacheRefsRootDir(), cfg.GetCacheRefsDir(digest))
	require.NoError(t, err)
	require.Equal(t, contentRel, refsRel)
}
