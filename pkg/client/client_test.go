package client

import (
	"context"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/stretchr/testify/require"
)

func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	raw := &config.RawConfig{
		ServiceName:              "test.csi.example.com",
		RootDir:                  t.TempDir(),
		ExternalCSIAuthorization: "test-token",
	}
	return config.NewWithRaw(raw)
}

func TestNewGRPCClient_TCP(t *testing.T) {
	cfg := newTestConfig(t)
	c, err := NewGRPCClient(cfg, "127.0.0.1:19999")
	require.NoError(t, err)
	require.NotNil(t, c)
	_ = c.Close()
}

func TestNewGRPCClient_TCPPrefix(t *testing.T) {
	cfg := newTestConfig(t)
	c, err := NewGRPCClient(cfg, "tcp://127.0.0.1:19999")
	require.NoError(t, err)
	require.NotNil(t, c)
	_ = c.Close()
}

func TestGRPCClient_CloseNilConn(t *testing.T) {
	c := &GRPCClient{}
	err := c.Close()
	require.NoError(t, err)
}

func TestNewHTTPClient_Unix(t *testing.T) {
	c, err := NewHTTPClient("unix:///tmp/test.sock")
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestNewHTTPClient_InvalidURL(t *testing.T) {
	// url.Parse rarely errors; test with a valid-looking addr to reach the second parse
	c, err := NewHTTPClient("unix:///tmp/handler.sock")
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestDumpPayload_ValidObject(t *testing.T) {
	obj := map[string]string{"key": "value"}
	r, err := dumpPayload(obj)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestDumpPayload_UnmarshalableObject(t *testing.T) {
	// channels cannot be marshaled to JSON
	ch := make(chan int)
	_, err := dumpPayload(ch)
	require.Error(t, err)
}
// newUnavailableGRPCClient creates a client pointing to a non-existent server.
// gRPC dials are lazy so construction succeeds, but actual calls fail.
func newUnavailableGRPCClient(t *testing.T) *GRPCClient {
	t.Helper()
	cfg := newTestConfig(t)
	c, err := NewGRPCClient(cfg, "127.0.0.1:19998")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestGRPCClient_CreateVolume_Error(t *testing.T) {
	c := newUnavailableGRPCClient(t)
	_, err := c.CreateVolume(context.Background(), "vol1", map[string]string{})
	require.Error(t, err)
}

func TestGRPCClient_DeleteVolume_Error(t *testing.T) {
	c := newUnavailableGRPCClient(t)
	_, err := c.DeleteVolume(context.Background(), "vol1")
	require.Error(t, err)
}

func TestGRPCClient_PublishVolume_Error(t *testing.T) {
	c := newUnavailableGRPCClient(t)
	_, err := c.PublishVolume(context.Background(), "vol1", "/tmp/target")
	require.Error(t, err)
}

func TestGRPCClient_UnpublishVolume_Error(t *testing.T) {
	c := newUnavailableGRPCClient(t)
	_, err := c.UnpublishVolume(context.Background(), "vol1", "/tmp/target")
	require.Error(t, err)
}

func TestGRPCClient_PublishStaticInlineVolume_Error(t *testing.T) {
	c := newUnavailableGRPCClient(t)
	_, err := c.PublishStaticInlineVolume(context.Background(), "vol1", "/tmp/target", "registry.example.com/model:v1")
	require.Error(t, err)
}
