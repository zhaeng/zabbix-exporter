package metadata

import (
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

// Store publishes complete metadata snapshots with one atomic pointer swap.
// Readers never take the apply lock.
type Store struct {
	applyMu  sync.Mutex
	snapshot atomic.Pointer[MetadataSnapshot]
}

func NewStore() *Store {
	store := &Store{}
	store.snapshot.Store(emptySnapshot())
	return store
}

func emptySnapshot() *MetadataSnapshot {
	return &MetadataSnapshot{
		Hosts:      map[string]*HostMetadata{},
		HostIDs:    []string{},
		Items:      map[string]*ItemMetadata{},
		HostItems:  map[string][]string{},
		Groups:     map[string]*CollectionGroup{},
		ItemGroups: map[string]string{},
	}
}

// Snapshot returns the current immutable view. Callers must not mutate it.
func (s *Store) Snapshot() *MetadataSnapshot {
	return s.snapshot.Load()
}

// ApplySnapshot deep-clones a complete candidate and publishes it atomically.
// Snapshot.Version advances for every successful complete apply. Definition
// versions advance only for new or materially changed host/item/group entries.
func (s *Store) ApplySnapshot(next *MetadataSnapshot) MetadataDiff {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	current := s.snapshot.Load()
	if next == nil {
		return MetadataDiff{OldVersion: current.Version, NewVersion: current.Version}
	}
	version := current.Version + 1
	createdAt := next.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	published := cloneSnapshot(next, current, version, createdAt)
	diff := diffSnapshots(current, published)
	s.snapshot.Store(published)
	return diff
}

func (s *Store) Item(itemID string) (*ItemMetadata, bool) {
	item, ok := s.snapshot.Load().Items[itemID]
	if !ok {
		return nil, false
	}
	return cloneItem(item, item.Version, item.RefreshedAt), true
}

func (s *Store) Group(groupID string) (*CollectionGroup, bool) {
	group, ok := s.snapshot.Load().Groups[groupID]
	if !ok {
		return nil, false
	}
	return cloneGroup(group, group.Version), true
}

func cloneSnapshot(source, current *MetadataSnapshot, snapshotVersion uint64, createdAt time.Time) *MetadataSnapshot {
	clone := &MetadataSnapshot{
		Version:    snapshotVersion,
		CreatedAt:  createdAt,
		Hosts:      make(map[string]*HostMetadata, len(source.Hosts)),
		Items:      make(map[string]*ItemMetadata, len(source.Items)),
		HostItems:  make(map[string][]string, len(source.HostItems)),
		Groups:     make(map[string]*CollectionGroup, len(source.Groups)),
		ItemGroups: make(map[string]string, len(source.ItemGroups)),
	}
	for id, host := range source.Hosts {
		publishedHost := cloneHost(host, snapshotVersion, createdAt)
		if previous, exists := current.Hosts[id]; exists && hostDefinitionsEqual(previous, publishedHost) && publishedHost != nil {
			publishedHost.Version = previous.Version
		}
		clone.Hosts[id] = publishedHost
	}
	clone.HostIDs = sortedHostIDs(clone.Hosts)
	for id, item := range source.Items {
		publishedItem := cloneItem(item, snapshotVersion, createdAt)
		if previous, exists := current.Items[id]; exists && itemDefinitionsEqual(previous, publishedItem) && publishedItem != nil {
			publishedItem.Version = previous.Version
		}
		clone.Items[id] = publishedItem
	}
	for hostID, itemIDs := range source.HostItems {
		clone.HostItems[hostID] = append([]string(nil), itemIDs...)
	}
	for id, group := range source.Groups {
		publishedGroup := cloneGroup(group, snapshotVersion)
		if previous, exists := current.Groups[id]; exists && groupDefinitionsEqual(previous, publishedGroup) && publishedGroup != nil {
			publishedGroup.Version = previous.Version
		}
		clone.Groups[id] = publishedGroup
	}
	for itemID, groupID := range source.ItemGroups {
		clone.ItemGroups[itemID] = groupID
	}
	return clone
}

func cloneHost(source *HostMetadata, version uint64, refreshedAt time.Time) *HostMetadata {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Inventory = cloneInventory(source.Inventory)
	clone.InventoryFields = cloneStringMap(source.InventoryFields)
	clone.Labels = cloneLabels(source.Labels)
	clone.RefreshedAt = refreshedAt
	clone.Version = version
	return &clone
}

func cloneItem(source *ItemMetadata, version uint64, refreshedAt time.Time) *ItemMetadata {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Labels = cloneLabels(source.Labels)
	clone.RefreshedAt = refreshedAt
	clone.Version = version
	return &clone
}

func cloneGroup(source *CollectionGroup, version uint64) *CollectionGroup {
	if source == nil {
		return nil
	}
	clone := *source
	clone.ItemIDs = append([]string(nil), source.ItemIDs...)
	clone.Version = version
	return &clone
}

func cloneLabels(source []prompb.Label) []prompb.Label {
	return append([]prompb.Label(nil), source...)
}

func diffSnapshots(old, next *MetadataSnapshot) MetadataDiff {
	diff := MetadataDiff{OldVersion: old.Version, NewVersion: next.Version}
	diff.AddedHosts, diff.RemovedHosts, diff.ChangedHosts = diffMap(old.Hosts, next.Hosts, hostDefinitionsEqual)
	diff.AddedItems, diff.RemovedItems, diff.ChangedItems = diffMap(old.Items, next.Items, itemDefinitionsEqual)
	diff.AddedGroups, diff.RemovedGroups, diff.ChangedGroups = diffMap(old.Groups, next.Groups, groupDefinitionsEqual)
	return diff
}

func diffMap[T any](old, next map[string]T, equal func(T, T) bool) (added, removed, changed []string) {
	for id, oldValue := range old {
		nextValue, ok := next[id]
		if !ok {
			removed = append(removed, id)
			continue
		}
		if !equal(oldValue, nextValue) {
			changed = append(changed, id)
		}
	}
	for id := range next {
		if _, ok := old[id]; !ok {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

func hostDefinitionsEqual(a, b *HostMetadata) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.HostID == b.HostID &&
		a.HostName == b.HostName &&
		a.DisplayName == b.DisplayName &&
		a.HostIP == b.HostIP &&
		reflect.DeepEqual(a.Inventory, b.Inventory) &&
		reflect.DeepEqual(a.InventoryFields, b.InventoryFields) &&
		reflect.DeepEqual(a.Labels, b.Labels)
}

func itemDefinitionsEqual(a, b *ItemMetadata) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ItemID == b.ItemID &&
		a.HostID == b.HostID &&
		a.Key == b.Key &&
		a.Name == b.Name &&
		a.ValueType == b.ValueType &&
		a.RawDelay == b.RawDelay &&
		a.Delay == b.Delay &&
		a.DelayParseState == b.DelayParseState &&
		a.Status == b.Status &&
		a.State == b.State &&
		a.Enabled == b.Enabled &&
		a.MetricName == b.MetricName &&
		reflect.DeepEqual(a.Labels, b.Labels)
}

func groupDefinitionsEqual(a, b *CollectionGroup) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID &&
		a.HostID == b.HostID &&
		a.ValueType == b.ValueType &&
		a.Delay == b.Delay &&
		reflect.DeepEqual(a.ItemIDs, b.ItemIDs)
}
