package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/modelpack/modctl/pkg/backend"
	pkgcodec "github.com/modelpack/modctl/pkg/codec"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestLayerAwarePuller_ConcurrencyDedup(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "http"}, nil
	})

	d1 := digest.FromString("layer1")
	layer1 := ocispec.Descriptor{
		Digest:    d1,
		Size:      9,
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Annotations: map[string]string{
			"org.cncf.model.filepath": "layer1.tar",
		},
	}
	manifest := ocispec.Manifest{Layers: []ocispec.Descriptor{layer1}}
	manifestBytes, _ := json.Marshal(manifest)

	var fetchCalls atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v2/test/repo/manifests/latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestBytes)
			return
		}
		if r.URL.Path == "/v2/test/repo/blobs/"+d1.String() {
			fetchCalls.Add(1)
			time.Sleep(50 * time.Millisecond) // Simulate network IO
			_, _ = w.Write([]byte("dummydata"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Strip http:// prefix
	registryHost := ts.URL[7:]
	refStr := registryHost + "/test/repo:latest"

	patches.ApplyFunc(pkgcodec.New, func(string) (pkgcodec.Codec, error) {
		return &dummyCodec{}, nil
	})

	lc := NewLayerCache(2)
	puller := &layerAwarePuller{
		pullCfg:    &config.PullConfig{Concurrency: 4},
		hook:       status.NewHook(context.Background()),
		layerCache: lc,
	}

	b, _ := backend.New("")
	artifact := &backend.InspectedModelArtifact{}

	const numPods = 10
	var wg sync.WaitGroup

	for i := 0; i < numPods; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			targetDir := filepath.Join(t.TempDir(), "model")
			err := puller.layerAwarePull(context.Background(), b, artifact, refStr, targetDir, true)
			require.NoError(t, err)
		}()
	}

	wg.Wait()

	require.Equal(t, int32(1), fetchCalls.Load(), "only one remote.Fetch should execute per digest")
}

func TestLayerAwarePuller_SemaphoreCancelNoLeak(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(auth.GetKeyChainByRef, func(string) (*auth.PassKeyChain, error) {
		return &auth.PassKeyChain{ServerScheme: "http"}, nil
	})

	d1 := digest.FromString("layer1")
	layer1 := ocispec.Descriptor{
		Digest:    d1,
		Size:      9,
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Annotations: map[string]string{
			"org.cncf.model.filepath": "layer1.tar",
		},
	}
	manifestBytes, _ := json.Marshal(ocispec.Manifest{Layers: []ocispec.Descriptor{layer1}})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v2/test/repo/manifests/latest" {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestBytes)
			return
		}
		if r.URL.Path == "/v2/test/repo/blobs/"+d1.String() {
			// Simulate a hung connection or slow download
			time.Sleep(2 * time.Second)
			_, _ = w.Write([]byte("dummydata"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	registryHost := ts.URL[7:]
	refStr := registryHost + "/test/repo:latest"

	patches.ApplyFunc(pkgcodec.New, func(string) (pkgcodec.Codec, error) {
		return &dummyCodec{}, nil
	})

	lc := NewLayerCache(2)
	puller := &layerAwarePuller{
		pullCfg:    &config.PullConfig{Concurrency: 4},
		hook:       status.NewHook(context.Background()),
		layerCache: lc,
	}

	b, _ := backend.New("")
	artifact := &backend.InspectedModelArtifact{}
	targetDir := filepath.Join(t.TempDir(), "model")

	// Create a context that will cancel quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := puller.layerAwarePull(ctx, b, artifact, refStr, targetDir, true)
	require.Error(t, err)

	// Verify semaphore is fully released.
	require.True(t, lc.Semaphore().TryAcquire(2), "semaphore should have 2 permits fully restored after failure")
}

type dummyCodec struct{}

func (d *dummyCodec) Type() string                             { return "dummy" }
func (d *dummyCodec) Encode(string, string) (io.Reader, error) { return nil, nil }
func (d *dummyCodec) Decode(outputDir, fp string, reader io.Reader, desc ocispec.Descriptor) error {
	fullPath := filepath.Join(outputDir, fp)
	_ = os.MkdirAll(filepath.Dir(fullPath), 0755)

	// Must consume reader to prevent connection aborts
	_, _ = io.Copy(io.Discard, reader)

	_ = os.WriteFile(fullPath, []byte("data"), 0644)
	return nil
}
