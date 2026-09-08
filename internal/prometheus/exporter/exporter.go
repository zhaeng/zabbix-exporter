package exporter

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/prompb"

	"github.com/zhaeng/zabbix-exporter/internal/cache"
	"github.com/zhaeng/zabbix-exporter/internal/metadata"
)

const defaultScrapePageSize = 5000

type MetadataSnapshotProvider interface {
	Snapshot() *metadata.MetadataSnapshot
}

// ValueExporter exposes the scheduled pipeline's shared ValueCache. It has no
// Zabbix dependency, so scraping /metrics cannot trigger an API request.
type ValueExporter struct {
	values   cache.ScrapeValueStore
	metadata MetadataSnapshotProvider
	pageSize int
	now      func() time.Time
}

func NewValueExporter(values cache.ScrapeValueStore, source MetadataSnapshotProvider, pageSize int) (*ValueExporter, error) {
	if values == nil {
		return nil, fmt.Errorf("scrape value store is required")
	}
	if source == nil {
		return nil, fmt.Errorf("metadata snapshot provider is required")
	}
	if pageSize == 0 {
		pageSize = defaultScrapePageSize
	}
	if pageSize < 1 {
		return nil, fmt.Errorf("scrape page size must be positive: %d", pageSize)
	}
	return &ValueExporter{values: values, metadata: source, pageSize: pageSize, now: time.Now}, nil
}

// Describe intentionally leaves ValueExporter unchecked because its metric
// descriptors are driven by the current immutable metadata snapshot.
func (e *ValueExporter) Describe(_ chan<- *prometheus.Desc) {}

func (e *ValueExporter) Collect(ch chan<- prometheus.Metric) {
	snapshot := e.metadata.Snapshot()
	if snapshot == nil {
		return
	}
	e.collectHosts(snapshot, ch)

	scrapeTimestamp := e.now()
	var cursor cache.PublishCursor
	for {
		page, err := e.values.ReadScrapePage(cursor, scrapeTimestamp, e.pageSize)
		if err != nil {
			return
		}
		for _, value := range page.Values {
			item := snapshot.Items[value.ItemID]
			if item == nil || item.Version != value.MetadataVersion {
				continue
			}
			name, labelNames, labelValues, ok := extractPromLabels(item.Labels)
			if !ok {
				continue
			}
			metric, err := prometheus.NewConstMetric(
				prometheus.NewDesc(name, "Zabbix metric", labelNames, nil),
				prometheus.GaugeValue,
				value.Value,
				labelValues...,
			)
			if err != nil {
				continue
			}
			ch <- prometheus.NewMetricWithTimestamp(value.Timestamp, metric)
		}
		if page.Done {
			return
		}
		if page.Next == cursor {
			return
		}
		cursor = page.Next
	}
}

// collectHosts deliberately emits no explicit timestamp. zabbix_host is
// metadata-derived rather than a history sample, so Prometheus owns the scrape
// timestamp on Pull.
func (e *ValueExporter) collectHosts(snapshot *metadata.MetadataSnapshot, ch chan<- prometheus.Metric) {
	for _, hostID := range snapshot.HostIDs {
		series, ok := metadata.ExportableHostSeries(hostID, snapshot.Hosts[hostID])
		if !ok {
			continue
		}
		name, labelNames, labelValues, ok := extractPromLabels(series.Labels)
		if !ok {
			continue
		}
		metric, err := prometheus.NewConstMetric(
			prometheus.NewDesc(name, "Zabbix host information", labelNames, nil),
			prometheus.GaugeValue,
			1,
			labelValues...,
		)
		if err != nil {
			continue
		}
		ch <- metric
	}
}

func extractPromLabels(labels []prompb.Label) (string, []string, []string, bool) {
	var name string
	names := make([]string, 0, len(labels))
	values := make([]string, 0, len(labels))
	for _, label := range labels {
		if label.Name == "__name__" {
			name = label.Value
			continue
		}
		names = append(names, label.Name)
		values = append(values, label.Value)
	}
	return name, names, values, name != ""
}
