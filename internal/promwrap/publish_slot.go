package promwrap

import (
	"fmt"

	"zabbix-exporter/internal/cache"
)

// SeriesFingerprint is stable across process restarts. The current data model
// maps one Zabbix item to one Prometheus series, so itemID is the canonical
// series key. If one item later fans out to multiple series, that migration
// must extend the key instead of changing this function in place.
func SeriesFingerprint(itemID string) uint64 {
	return cache.SeriesFingerprint(itemID)
}

// PublishSlotForSeries returns the stable minute-spread slot for a series.
func PublishSlotForSeries(itemID string, slotCount uint16) (uint16, error) {
	if slotCount == 0 {
		return 0, fmt.Errorf("publish slot count must be positive")
	}
	return uint16(SeriesFingerprint(itemID) % uint64(slotCount)), nil
}

func workerLaneForSeries(itemID string, workerCount int) int {
	return int(SeriesFingerprint(itemID) % uint64(workerCount))
}
