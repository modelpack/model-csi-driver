package metrics

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

type MountItem struct {
    Reference  string
    Type       string
    VolumeName string
    MountID    string
}

type MountItemCollector struct {
    desc  *prometheus.Desc
    items atomic.Value // stores []MountItem
}

func NewMountItemCollector() *MountItemCollector {
    c := &MountItemCollector{
        desc: prometheus.NewDesc(
            Prefix+"mount_item",
            "Mounted item list (pvc, inline, dynamic types), value is always 1 for existing items.",
            []string{"reference", "type", "volume_name", "mount_id"},
            nil,
        ),
    }
    c.items.Store([]MountItem(nil))
    return c
}

func (c *MountItemCollector) Set(items []MountItem) {
    c.items.Store(append([]MountItem(nil), items...))
}

func (c *MountItemCollector) Describe(ch chan<- *prometheus.Desc) {
    ch <- c.desc
}

func (c *MountItemCollector) Collect(ch chan<- prometheus.Metric) {
    v := c.items.Load()
    if v == nil {
        return
    }
    items := v.([]MountItem)
    for _, it := range items {
        ch <- prometheus.MustNewConstMetric(
            c.desc,
            prometheus.GaugeValue,
            1,
            it.Reference,
            it.Type,
            it.VolumeName,
            it.MountID,
        )
    }
}

var MountItems = NewMountItemCollector()
