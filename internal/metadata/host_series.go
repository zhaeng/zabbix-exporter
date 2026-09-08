package metadata

import (
	"sort"

	"github.com/cespare/xxhash/v2"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
)

const (
	HostMetricName      = "zabbix_host"
	hostSeriesKeyPrefix = "host\x00"
)

// HostSeries is a detached identity plus immutable labels borrowed from one
// MetadataSnapshot. Callers must not mutate Labels.
type HostSeries struct {
	HostID      string
	Key         string
	Labels      []prompb.Label
	Fingerprint uint64
	Version     uint64
}

// ExportableHostSeries is the shared Pull/Push construction and validation
// path for zabbix_host. HostID, rather than labels, is the stable identity so a
// metadata label refresh does not move the series between publish slots.
func ExportableHostSeries(hostID string, host *HostMetadata) (HostSeries, bool) {
	if host == nil || hostID == "" || host.HostID != hostID || host.HostIP == "" {
		return HostSeries{}, false
	}
	if !validHostLabels(host.Labels) {
		return HostSeries{}, false
	}
	key := hostSeriesKeyPrefix + hostID
	return HostSeries{
		HostID:      hostID,
		Key:         key,
		Labels:      host.Labels,
		Fingerprint: HostSeriesFingerprint(hostID),
		Version:     host.Version,
	}, true
}

// HostSeriesFingerprint is stable across process restarts and metadata label
// changes. The namespace keeps host identities distinct from item identities.
func HostSeriesFingerprint(hostID string) uint64 {
	return xxhash.Sum64String(hostSeriesKeyPrefix + hostID)
}

func validHostLabels(labels []prompb.Label) bool {
	var hasName, hasHost, hasHostIP, hasVMID bool
	for index, label := range labels {
		if label.Name == "" || !model.LabelName(label.Name).IsValid() || !model.LabelValue(label.Value).IsValid() {
			return false
		}
		if index != 0 && labels[index-1].Name >= label.Name {
			return false
		}
		switch label.Name {
		case "__name__":
			hasName = label.Value == HostMetricName && model.IsValidMetricName(model.LabelValue(label.Value))
		case "host":
			hasHost = label.Value != ""
		case "host_ip":
			hasHostIP = label.Value != ""
		case "vm_id":
			hasVMID = label.Value != ""
		}
	}
	return hasName && hasHost && hasHostIP && hasVMID
}

func sortedHostIDs(hosts map[string]*HostMetadata) []string {
	ids := make([]string, 0, len(hosts))
	for hostID := range hosts {
		ids = append(ids, hostID)
	}
	sort.Strings(ids)
	return ids
}
