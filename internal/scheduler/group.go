package scheduler

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"time"

	"github.com/zhaeng/zabbix-exporter/internal/metadata"
)

const DefaultBatchSize = 50

// Batch is a stable history.get unit. Its ID changes when its membership
// changes, which prevents a watermark from being reused for a different set of
// items after metadata reconciliation.
type Batch struct {
	ID      string
	ItemIDs []string
}

// Group contains only immutable scheduling input. Runtime state is owned by
// Scheduler and per-batch watermarks are owned by collector.GroupCollector.
type Group struct {
	ID        string
	HostID    string
	ValueType int
	Delay     time.Duration
	Batches   []Batch
	Version   uint64
}

// GroupBuilder deterministically groups numeric items by host, value type and
// delay, then partitions sorted item IDs into bounded batches.
type GroupBuilder struct {
	batchSize int
}

func NewGroupBuilder(batchSize int) (*GroupBuilder, error) {
	if batchSize == 0 {
		batchSize = DefaultBatchSize
	}
	if batchSize < 1 {
		return nil, fmt.Errorf("history batch size must be positive: %d", batchSize)
	}
	return &GroupBuilder{batchSize: batchSize}, nil
}

func (b *GroupBuilder) Build(snapshot *metadata.MetadataSnapshot) []Group {
	if snapshot == nil {
		return nil
	}

	type groupKey struct {
		hostID    string
		valueType int
		delay     time.Duration
	}
	groupItems := make(map[groupKey][]string)
	for itemID, item := range snapshot.Items {
		if item == nil || !item.Enabled || item.HostID == "" || item.Delay <= 0 {
			continue
		}
		valueType, err := strconv.Atoi(item.ValueType)
		if err != nil || (valueType != 0 && valueType != 3) {
			continue
		}
		groupItems[groupKey{hostID: item.HostID, valueType: valueType, delay: item.Delay}] = append(
			groupItems[groupKey{hostID: item.HostID, valueType: valueType, delay: item.Delay}], itemID,
		)
	}

	groups := make([]Group, 0, len(groupItems))
	for key, itemIDs := range groupItems {
		sort.Strings(itemIDs)
		itemIDs = compactStrings(itemIDs)
		groupID := GroupID(key.hostID, key.valueType, key.delay)
		group := Group{
			ID:        groupID,
			HostID:    key.hostID,
			ValueType: key.valueType,
			Delay:     key.delay,
		}
		if source := snapshot.Groups[groupID]; source != nil {
			group.Version = source.Version
		} else {
			group.Version = snapshot.Version
		}
		for start := 0; start < len(itemIDs); start += b.batchSize {
			end := start + b.batchSize
			if end > len(itemIDs) {
				end = len(itemIDs)
			}
			ids := append([]string(nil), itemIDs[start:end]...)
			group.Batches = append(group.Batches, Batch{
				ID:      batchID(groupID, ids),
				ItemIDs: ids,
			})
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups
}

// GroupID matches metadata.Builder's stable host/valueType/delay key.
func GroupID(hostID string, valueType int, delay time.Duration) string {
	return hostID + "/" + strconv.Itoa(valueType) + "/" + strconv.FormatInt(int64(delay/time.Second), 10)
}

func batchID(groupID string, itemIDs []string) string {
	hash := fnv.New64a()
	for _, itemID := range itemIDs {
		_, _ = hash.Write([]byte(itemID))
		_, _ = hash.Write([]byte{0})
	}
	return groupID + "/batch-" + strconv.FormatUint(hash.Sum64(), 16)
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

// CollectionPeriod prevents sub-minute items from causing more than one
// scheduled collection per minute while preserving longer configured delays.
func CollectionPeriod(delay time.Duration) time.Duration {
	if delay <= time.Minute {
		return time.Minute
	}
	return delay
}

// StableOffset deterministically spreads a group inside the supplied window.
func StableOffset(groupID string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(groupID))
	return time.Duration(hash.Sum64() % uint64(window))
}

func cloneGroup(group Group) Group {
	clone := group
	clone.Batches = make([]Batch, len(group.Batches))
	for i := range group.Batches {
		clone.Batches[i] = Batch{
			ID:      group.Batches[i].ID,
			ItemIDs: append([]string(nil), group.Batches[i].ItemIDs...),
		}
	}
	return clone
}
