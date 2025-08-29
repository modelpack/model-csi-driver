package metrics

import (
	"sort"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	DummyRegistry  = prometheus.NewRegistry()
	DetailRegistry = prometheus.NewRegistry()
	Registry       = prometheus.NewRegistry()
	Prefix         = "model_csi_"

	sizeLabel = "size_in_mb"
	opLabel   = "op"
)

var LatencyInSecondsBuckets = prometheus.ExponentialBuckets(1, 2, 16)
var SizeInMBBuckets = prometheus.ExponentialBuckets(1, 2, 24)

func getSizeLabel(sizeInBytes int64) prometheus.Labels {
	sizeInMB := float64(sizeInBytes) / (1024 * 1024)

	sizeIndex := sort.SearchFloat64s(SizeInMBBuckets, sizeInMB)
	sizeInMBStr := "+Inf"
	if sizeIndex < len(SizeInMBBuckets) {
		sizeInMBStr = humanize.IBytes(uint64(SizeInMBBuckets[sizeIndex] * 1024 * 1024))
	}

	return prometheus.Labels{sizeLabel: sizeInMBStr}
}

var (
	NodeNotReady = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: Prefix + "node_not_ready",
		},
	)

	NodeOpFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: Prefix + "node_op_failed",
		},
		[]string{opLabel},
	)

	NodeOpSucceed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: Prefix + "node_op_succeed",
		},
		[]string{opLabel},
	)

	NodePullLayerTooLong = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: Prefix + "node_pull_layer_too_long",
		},
	)

	NodeOpLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    Prefix + "node_op_latency_in_seconds",
		Buckets: LatencyInSecondsBuckets,
	}, []string{opLabel})

	NodePullOpLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    Prefix + "node_pull_op_latency_in_seconds",
		Buckets: LatencyInSecondsBuckets,
	}, []string{opLabel, sizeLabel})

	NodeCacheSizeInBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: Prefix + "node_cache_size_in_bytes",
		},
	)

	NodeMountedStaticImages = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: Prefix + "node_mounted_static_images",
		},
	)

	NodeMountedDynamicImages = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: Prefix + "node_mounted_dynamic_images",
		},
	)

	ControllerOpFailed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: Prefix + "controller_op_failed",
		},
		[]string{opLabel},
	)

	ControllerOpSucceed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: Prefix + "controller_op_succeed",
		},
		[]string{opLabel},
	)

	ControllerOpLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    Prefix + "controller_op_latency_in_seconds",
		Buckets: LatencyInSecondsBuckets,
	}, []string{opLabel})
)

func NodeOpObserve(op string, start time.Time, err error) {
	if err != nil {
		NodeOpFailed.With(prometheus.Labels{opLabel: op}).Inc()
	} else {
		NodeOpSucceed.With(prometheus.Labels{opLabel: op}).Inc()
		NodeOpLatency.With(prometheus.Labels{opLabel: op}).Observe(time.Since(start).Seconds())
	}
}

func ControllerOpObserve(op string, start time.Time, err error) {
	if err != nil {
		ControllerOpFailed.With(prometheus.Labels{opLabel: op}).Inc()
	} else {
		ControllerOpSucceed.With(prometheus.Labels{opLabel: op}).Inc()
		ControllerOpLatency.With(prometheus.Labels{opLabel: op}).Observe(time.Since(start).Seconds())
	}
}

func NodePullOpObserve(op string, size int64, start time.Time, err error) {
	if err != nil {
		NodeOpFailed.With(prometheus.Labels{opLabel: op}).Inc()
	} else {
		NodeOpSucceed.With(prometheus.Labels{opLabel: op}).Inc()
		labels := getSizeLabel(size)
		labels[opLabel] = op
		NodePullOpLatency.With(labels).Observe(time.Since(start).Seconds())
	}
}

func init() {
	DummyRegistry.MustRegister()

	DetailRegistry.MustRegister()

	Registry.MustRegister(
		NodeNotReady,

		NodeOpFailed,
		NodeOpSucceed,
		NodeOpLatency,
		NodePullOpLatency,

		ControllerOpFailed,
		ControllerOpSucceed,
		ControllerOpLatency,

		NodeCacheSizeInBytes,
		NodeMountedStaticImages,
		NodeMountedDynamicImages,
		NodePullLayerTooLong,
	)
}
