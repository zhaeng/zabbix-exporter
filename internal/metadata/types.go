// Package metadata owns immutable Zabbix host/item definitions. It deliberately
// contains no metric values or scheduler state.
package metadata

import (
	"context"
	"time"

	"github.com/zhaeng/zabbix-exporter/internal/zabbix"

	"github.com/prometheus/prometheus/prompb"
)

// DelayParseStatus records whether Delay is an exact Zabbix simple interval or
// a configured fallback. Callers must not treat a fallback as the source value.
type DelayParseStatus string

const (
	DelayParsed            DelayParseStatus = "parsed"
	DelayFallbackMissing   DelayParseStatus = "fallback_missing"
	DelayFallbackInvalid   DelayParseStatus = "fallback_invalid"
	DelayFallbackFlexible  DelayParseStatus = "fallback_flexible"
	DelayFallbackScheduled DelayParseStatus = "fallback_scheduled"
	DelayFallbackMacro     DelayParseStatus = "fallback_macro"
)

// HostMetadata is the value-free definition of a Zabbix host.
type HostMetadata struct {
	HostID          string
	HostName        string
	DisplayName     string
	HostIP          string
	Inventory       *zabbix.HostInventory
	InventoryFields map[string]string
	Labels          []prompb.Label
	RefreshedAt     time.Time
	Version         uint64
}

// ItemMetadata is the value-free definition of a numeric Zabbix item. It must
// never grow dynamic item values or sample timestamps.
type ItemMetadata struct {
	ItemID          string
	HostID          string
	Key             string
	Name            string
	ValueType       string
	RawDelay        string
	Delay           time.Duration
	DelayParseState DelayParseStatus
	Status          string
	State           string
	Enabled         bool
	MetricName      string
	Labels          []prompb.Label
	RefreshedAt     time.Time
	Version         uint64
}

// CollectionGroup is an immutable collection definition. Runtime fields such
// as next-run, watermark, and in-flight state belong to the future scheduler.
type CollectionGroup struct {
	ID        string
	HostID    string
	ValueType string
	Delay     time.Duration
	ItemIDs   []string
	Version   uint64
}

// MetadataSnapshot is read-only after publication. Maps and slices are fully
// built off-path, cloned by Store, and then atomically published as one version.
type MetadataSnapshot struct {
	Version   uint64
	CreatedAt time.Time
	Hosts     map[string]*HostMetadata
	// HostIDs is a sorted, derived traversal index over Hosts. Hosts remains
	// the only source of host definitions; Store rebuilds this index for every
	// immutable published snapshot.
	HostIDs    []string
	Items      map[string]*ItemMetadata
	HostItems  map[string][]string
	Groups     map[string]*CollectionGroup
	ItemGroups map[string]string
}

// MetadataDiff describes a transition between two complete snapshots. Sorted
// IDs make downstream application deterministic.
type MetadataDiff struct {
	OldVersion uint64
	NewVersion uint64

	AddedHosts   []string
	RemovedHosts []string
	ChangedHosts []string

	AddedItems   []string
	RemovedItems []string
	ChangedItems []string

	AddedGroups   []string
	RemovedGroups []string
	ChangedGroups []string
}

// MetadataStore exposes the atomic, read-only metadata view used by later
// Value Cache and scheduler tasks.
type MetadataStore interface {
	Snapshot() *MetadataSnapshot
	ApplySnapshot(next *MetadataSnapshot) MetadataDiff
	Item(itemID string) (*ItemMetadata, bool)
	Group(groupID string) (*CollectionGroup, bool)
}

// Reader is the minimal low-frequency Zabbix API surface used by Refresher.
// zabbix.Client implements it without exposing value-fetching methods here.
type Reader interface {
	GetHosts(ctx context.Context) ([]zabbix.Host, error)
	GetItemMetadata(ctx context.Context, hostIDs []string) ([]zabbix.Item, error)
}
