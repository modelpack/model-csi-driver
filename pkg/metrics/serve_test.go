package metrics

import (
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// ─── GetAddrByEnv ─────────────────────────────────────────────────────────────

func TestGetAddrByEnv_Local(t *testing.T) {
	addr := GetAddrByEnv("tcp://$POD_IP:5244", true)
	require.Equal(t, "tcp://127.0.0.1:5244", addr)
}

func TestGetAddrByEnv_WithPodIPEnv(t *testing.T) {
	require.NoError(t, os.Setenv(EnvPodIP, "10.0.0.1"))
	defer func() { _ = os.Unsetenv(EnvPodIP) }()

	addr := GetAddrByEnv("tcp://$POD_IP:5244", false)
	require.Equal(t, "tcp://10.0.0.1:5244", addr)
}

func TestGetAddrByEnv_DefaultHost(t *testing.T) {
	_ = os.Unsetenv(EnvPodIP)
	addr := GetAddrByEnv("tcp://$POD_IP:5244", false)
	require.Equal(t, "tcp://0.0.0.0:5244", addr)
}

func TestGetAddrByEnv_NoPlaceholder(t *testing.T) {
	addr := GetAddrByEnv("tcp://127.0.0.1:9090", false)
	require.Equal(t, "tcp://127.0.0.1:9090", addr)
}

// ─── NewServer ────────────────────────────────────────────────────────────────

func TestNewServer_EmptyAddr(t *testing.T) {
	_, err := NewServer("")
	require.Error(t, err)
}

func TestNewServer_ValidAddr(t *testing.T) {
	srv, err := NewServer("tcp://127.0.0.1:0")
	require.NoError(t, err)
	require.NotNil(t, srv)

	stop := make(chan struct{})
	go srv.Serve(stop)
	time.Sleep(10 * time.Millisecond)
	close(stop)
	time.Sleep(50 * time.Millisecond)
}

func TestNewServer_InvalidPort(t *testing.T) {
	// port 99999 is out of range.
	_, err := NewServer("tcp://127.0.0.1:99999")
	require.Error(t, err)
}

// ─── MountItemCollector ───────────────────────────────────────────────────────

func TestMountItemCollector_SetAndCollect(t *testing.T) {
	c := NewMountItemCollector()
	items := []MountItem{
		{Reference: "reg/model:v1", Type: "pvc", VolumeName: "pvc-vol", MountID: ""},
		{Reference: "reg/model:v2", Type: "dynamic", VolumeName: "csi-vol", MountID: "mount-1"},
	}
	c.Set(items)

	// Describe
	descCh := make(chan *prometheus.Desc, 5)
	c.Describe(descCh)
	close(descCh)
	var descs []*prometheus.Desc
	for d := range descCh {
		descs = append(descs, d)
	}
	require.Len(t, descs, 1)

	// Collect
	metricCh := make(chan prometheus.Metric, 10)
	c.Collect(metricCh)
	close(metricCh)
	var mets []prometheus.Metric
	for m := range metricCh {
		mets = append(mets, m)
	}
	require.Len(t, mets, 2)
}

func TestMountItemCollector_Empty(t *testing.T) {
	c := NewMountItemCollector()
	// Collect on empty should not produce any metrics, not panic.
	metricCh := make(chan prometheus.Metric, 5)
	c.Collect(metricCh)
	close(metricCh)
	var mets []prometheus.Metric
	for m := range metricCh {
		mets = append(mets, m)
	}
	require.Empty(t, mets)
}
