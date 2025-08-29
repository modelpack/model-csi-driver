package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestGetSizeLabel(t *testing.T) {
	require.Equal(t, prometheus.Labels{sizeLabel: "1.0 MiB"}, getSizeLabel(0))
	require.Equal(t, prometheus.Labels{sizeLabel: "1.0 MiB"}, getSizeLabel(1023))
	require.Equal(t, prometheus.Labels{sizeLabel: "1.0 MiB"}, getSizeLabel(1024))
	require.Equal(t, prometheus.Labels{sizeLabel: "1.0 MiB"}, getSizeLabel(1024*1024))
	require.Equal(t, prometheus.Labels{sizeLabel: "2.0 MiB"}, getSizeLabel(1024*1024+1))
	require.Equal(t, prometheus.Labels{sizeLabel: "1.0 TiB"}, getSizeLabel(1024*1024*1024*1024))
	require.Equal(t, prometheus.Labels{sizeLabel: "8.0 TiB"}, getSizeLabel(1024*1024*1024*1024*8))
	require.Equal(t, prometheus.Labels{sizeLabel: "+Inf"}, getSizeLabel(1024*1024*1024*1024*8+1))
}
