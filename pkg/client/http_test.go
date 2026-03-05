package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/stretchr/testify/require"
)

// setupUnixServer creates a test unix socket HTTP server using httptest and returns the socket path.
func setupTestHTTPServer(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	sockPath := fmt.Sprintf("%s/test-%d.sock", t.TempDir(), os.Getpid())

	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() { _ = srv.Close() })
	return sockPath
}

func TestHTTPClient_CreateMount(t *testing.T) {
	mux := http.NewServeMux()
	expectedStatus := status.Status{
		VolumeName: "vol1",
		MountID:    "m1",
		Reference:  "test/model:latest",
		State:      status.StateMounted,
	}
	mux.HandleFunc("/api/v1/volumes/vol1/mounts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(expectedStatus)
	})

	sockPath := setupTestHTTPServer(t, mux)
	client, err := NewHTTPClient("unix://" + sockPath)
	require.NoError(t, err)

	result, err := client.CreateMount(context.Background(), "vol1", "m1", "test/model:latest", false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "vol1", result.VolumeName)
}

func TestHTTPClient_GetMount(t *testing.T) {
	mux := http.NewServeMux()
	expectedStatus := status.Status{
		VolumeName: "vol1",
		MountID:    "m1",
		Reference:  "test/model:latest",
	}
	mux.HandleFunc("/api/v1/volumes/vol1/mounts/m1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(expectedStatus)
	})

	sockPath := setupTestHTTPServer(t, mux)
	client, err := NewHTTPClient("unix://" + sockPath)
	require.NoError(t, err)

	result, err := client.GetMount(context.Background(), "vol1", "m1")
	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestHTTPClient_DeleteMount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/volumes/vol1/mounts/m1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	sockPath := setupTestHTTPServer(t, mux)
	client, err := NewHTTPClient("unix://" + sockPath)
	require.NoError(t, err)

	err = client.DeleteMount(context.Background(), "vol1", "m1")
	require.NoError(t, err)
}

func TestHTTPClient_ListMounts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/volumes/vol1/mounts", func(w http.ResponseWriter, r *http.Request) {
		items := []status.Status{{VolumeName: "vol1", MountID: "m1"}}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(items)
	})

	sockPath := setupTestHTTPServer(t, mux)
	client, err := NewHTTPClient("unix://" + sockPath)
	require.NoError(t, err)

	items, err := client.ListMounts(context.Background(), "vol1")
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestHTTPClient_ServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/volumes/vol1/mounts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, "internal server error", http.StatusInternalServerError)
	})

	sockPath := setupTestHTTPServer(t, mux)
	client, err := NewHTTPClient("unix://" + sockPath)
	require.NoError(t, err)

	// CreateMount returning an error from the server should propagate
	_, err = client.CreateMount(context.Background(), "vol1", "m1", "ref", false)
	require.Error(t, err)
}

// Test request() with HTML content-type response (broken api endpoint)
func TestHTTPClient_Request_HTMLResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "<html>broken</html>")
	}))
	defer srv.Close()

	baseURL, err := url.Parse("http://unix")
	require.NoError(t, err)

	httpClient := &HTTPClient{
		baseURL: *baseURL,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("tcp", srv.Listener.Addr().String())
				},
			},
		},
	}

	_, err = httpClient.request(context.Background(), http.MethodGet, "/test", nil, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "broken api endpoint")
}

