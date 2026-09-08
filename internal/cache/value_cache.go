package cache

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zhaeng/zabbix-exporter/internal/metadata"
	"github.com/zhaeng/zabbix-exporter/internal/metrics"
	"github.com/zhaeng/zabbix-exporter/internal/zabbix"
)

const (
	MinValueCacheShards     = 64
	MaxValueCacheShards     = 256
	DefaultValueCacheShards = 256
	DefaultPublishSlots     = 12
	DefaultMissThreshold    = 2
)

// ValueCacheConfig fixes memory partitioning and state-machine defaults.
type ValueCacheConfig struct {
	ShardCount      int
	PublishSlots    int
	InitialCapacity int
	MissThreshold   uint32
	ExpiryRegistrar ExpiryRegistrar
	// MetadataSource is optional. Scheduled collection supplies it so an
	// in-flight history response captured from an older metadata definition
	// cannot recreate an entry after refresh reconciliation deleted it.
	MetadataSource MetadataSnapshotSource
}

type MetadataSnapshotSource interface {
	Snapshot() *metadata.MetadataSnapshot
}

// ValueStore is the T05/T06/T07 integration contract. ApplyBatch never turns a
// non-complete result into a miss. For a complete result, callers pass the
// returned MissItemIDs to MarkMiss exactly once. All reads are detached values.
type ValueStore interface {
	ApplyBatch(result zabbix.BatchResult, snapshot *metadata.MetadataSnapshot) ApplyResult
	MarkMiss(itemIDs []string, fetchedAt time.Time) MissResult
	Delete(itemIDs []string, reason DeleteReason) DeleteResult
	DeleteVersioned(items []VersionedItem, reason DeleteReason) DeleteResult
	Load(itemID string) (ValueState, bool)
	ReadPublishPage(slot uint16, cursor PublishCursor, cycleTimestamp time.Time, limit int) (PublishPage, error)
	AckPublished(acks []PublishAck) AckResult
	Len() int
}

// ScrapeValueStore is the ack-independent Pull view of ValueCache. It shares
// validity and expiry semantics with streaming Push, but a successful Push
// must never hide a still-valid value from /metrics.
type ScrapeValueStore interface {
	ReadScrapePage(cursor PublishCursor, scrapeTimestamp time.Time, limit int) (PublishPage, error)
}

type ApplyError struct {
	ItemID string
	Err    error
}

type ApplyResult struct {
	Status          zabbix.BatchStatus
	Created         int
	Updated         int
	Unchanged       int
	Rejected        int
	Skipped         int
	Stale           int
	MissItemIDs     []string
	Errors          []ApplyError
	MetadataVersion uint64
}

type MissResult struct {
	Marked      int
	Invalidated int
	NotFound    int
}

type DeleteReason uint8

const (
	DeleteMetadataRemoved DeleteReason = iota
	DeleteMetadataChanged
	DeleteExpired
	DeleteManual
)

func (r DeleteReason) String() string {
	switch r {
	case DeleteMetadataRemoved:
		return "metadata_removed"
	case DeleteMetadataChanged:
		return "metadata_changed"
	case DeleteExpired:
		return "expired"
	case DeleteManual:
		return "manual"
	default:
		return "unknown"
	}
}

type DeleteResult struct {
	Reason   DeleteReason
	Deleted  int
	Stale    int
	NotFound int
}

// VersionedItem is the race-safe deletion primitive used by the future expiry
// wheel. T04 only supplies the sharded/version-checked delete; it does not scan,
// schedule, or register expiry work.
type VersionedItem struct {
	ItemID       string
	ValueVersion uint64
}

// PublishCursor is an opaque-in-practice, stable cursor over shard insertion
// sequences. Its zero value starts a scan. Shard == cache shard count means EOF.
type PublishCursor struct {
	Shard    uint16
	Sequence uint64
}

// PublishValue is a detached page entry. Timestamp is already selected from
// SourceTimestamp (short cycle) or cycleTimestamp (long cycle).
type PublishValue struct {
	ItemID          string
	Value           float64
	Timestamp       time.Time
	SourceTimestamp time.Time
	SourceNS        int64
	MetadataVersion uint64
	ValueVersion    uint64
	PublishSlot     uint16
	PublishPolicy   PublishPolicy
}

type PublishPage struct {
	Values []PublishValue
	Next   PublishCursor
	Done   bool
}

// PublishAck must only be submitted after the corresponding remote write has
// succeeded. PublishedTimestamp is the sample timestamp carried by that write.
type PublishAck struct {
	ItemID             string
	ValueVersion       uint64
	PublishedTimestamp time.Time
}

type AckResult struct {
	Applied  int
	Stale    int
	NotFound int
}

// ValueCacheStats is a point-in-time aggregate over the sharded cache. Fresh
// entries are both valid and before their hard source-derived expiry time.
type ValueCacheStats struct {
	Entries int
	Valid   int
	Fresh   int
}

type ValueCache struct {
	shards           []valueShard
	shardMask        uint64
	publishSlots     uint16
	missThreshold    uint32
	expiryRegistrar  ExpiryRegistrar
	metadataSource   MetadataSnapshotSource
	nextValueVersion atomic.Uint64
	entries          atomic.Int64
}

var _ ValueStore = (*ValueCache)(nil)

func NewValueCache(config ValueCacheConfig) (*ValueCache, error) {
	if config.ShardCount == 0 {
		config.ShardCount = DefaultValueCacheShards
	}
	if config.PublishSlots == 0 {
		config.PublishSlots = DefaultPublishSlots
	}
	if config.MissThreshold == 0 {
		config.MissThreshold = DefaultMissThreshold
	}
	if config.ShardCount < MinValueCacheShards || config.ShardCount > MaxValueCacheShards || config.ShardCount&(config.ShardCount-1) != 0 {
		return nil, fmt.Errorf("value cache shard count must be a power of two in [%d,%d]: %d", MinValueCacheShards, MaxValueCacheShards, config.ShardCount)
	}
	if config.PublishSlots < 1 || config.PublishSlots > math.MaxUint16 {
		return nil, fmt.Errorf("publish slots must be in [1,%d]: %d", math.MaxUint16, config.PublishSlots)
	}
	if config.InitialCapacity < 0 {
		return nil, fmt.Errorf("initial capacity must not be negative: %d", config.InitialCapacity)
	}

	capacityPerShard := (config.InitialCapacity + config.ShardCount - 1) / config.ShardCount
	cache := &ValueCache{
		shards:          make([]valueShard, config.ShardCount),
		shardMask:       uint64(config.ShardCount - 1),
		publishSlots:    uint16(config.PublishSlots),
		missThreshold:   config.MissThreshold,
		expiryRegistrar: config.ExpiryRegistrar,
		metadataSource:  config.MetadataSource,
	}
	for i := range cache.shards {
		cache.shards[i] = newValueShard(capacityPerShard)
	}
	return cache, nil
}

func NewDefaultValueCache() *ValueCache {
	cache, err := NewValueCache(ValueCacheConfig{})
	if err != nil {
		panic(err)
	}
	return cache
}

type pendingValue struct {
	itemID          string
	value           float64
	clock           int64
	ns              int64
	delay           time.Duration
	metadataVersion uint64
	fetchedAt       time.Time
}

type applyOutcome struct {
	itemID    string
	created   bool
	updated   bool
	unchanged bool
}

func (c *ValueCache) ApplyBatch(result zabbix.BatchResult, snapshot *metadata.MetadataSnapshot) ApplyResult {
	apply := ApplyResult{Status: result.Status}
	if snapshot != nil {
		apply.MetadataVersion = snapshot.Version
	}
	if result.Status != zabbix.BatchSuccess && result.Status != zabbix.BatchEmpty {
		observeApplyResult(apply)
		return apply
	}
	if snapshot == nil {
		apply.Skipped = len(result.Latest)
		if apply.Skipped == 0 {
			apply.Skipped = len(result.Records)
		}
		observeApplyResult(apply)
		return apply
	}

	latest := result.Latest
	if latest == nil {
		latest = zabbix.ReduceLatest(result.Records)
	}
	returnedItems := make(map[string]struct{}, len(result.Records)+len(latest))
	for _, record := range result.Records {
		returnedItems[record.ItemID] = struct{}{}
	}
	for itemID := range latest {
		returnedItems[itemID] = struct{}{}
	}
	fetchedAt := result.FinishedAt
	if fetchedAt.IsZero() {
		fetchedAt = result.StartedAt
	}

	buckets := make([][]pendingValue, len(c.shards))
	touched := make([]int, 0)
	touchedSet := make([]bool, len(c.shards))
	protectedFromMiss := make(map[string]struct{}, len(latest))
	staleItems := make(map[string]struct{})
	for _, itemID := range result.Query.ItemIDs {
		item := snapshot.Items[itemID]
		if item == nil || !c.metadataVersionCurrent(itemID, item.Version) {
			if _, duplicate := staleItems[itemID]; !duplicate {
				staleItems[itemID] = struct{}{}
				apply.Stale++
			}
			protectedFromMiss[itemID] = struct{}{}
		}
	}
	for itemID, record := range latest {
		item, ok := snapshot.Items[itemID]
		if !ok || item == nil || !item.Enabled || item.Delay <= 0 || (item.ValueType != "0" && item.ValueType != "3") {
			apply.Skipped++
			protectedFromMiss[itemID] = struct{}{}
			continue
		}
		value, err := strconv.ParseFloat(record.Value, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			if err == nil {
				err = errors.New("value must be finite")
			}
			apply.Rejected++
			apply.Errors = append(apply.Errors, ApplyError{ItemID: itemID, Err: err})
			protectedFromMiss[itemID] = struct{}{}
			continue
		}
		ns := int64(record.NS)
		if ns < 0 || ns >= int64(time.Second) {
			apply.Rejected++
			apply.Errors = append(apply.Errors, ApplyError{ItemID: itemID, Err: fmt.Errorf("source ns out of range: %d", ns)})
			protectedFromMiss[itemID] = struct{}{}
			continue
		}
		pending := pendingValue{
			itemID:          itemID,
			value:           value,
			clock:           int64(record.Clock),
			ns:              ns,
			delay:           item.Delay,
			metadataVersion: item.Version,
			fetchedAt:       fetchedAt,
		}
		shardIndex := c.shardIndex(itemID)
		buckets[shardIndex] = append(buckets[shardIndex], pending)
		if !touchedSet[shardIndex] {
			touchedSet[shardIndex] = true
			touched = append(touched, shardIndex)
		}
	}

	sort.Ints(touched)
	outcomes := make(map[string]applyOutcome, len(latest))
	registrar := c.expiryRegistrar
	var expiryEntries []ExpiryEntry
	if registrar != nil {
		expiryEntries = make([]ExpiryEntry, 0, len(latest))
	}
	for _, shardIndex := range touched {
		shard := &c.shards[shardIndex]
		shard.mu.Lock()
		for _, pending := range buckets[shardIndex] {
			if !c.metadataVersionCurrent(pending.itemID, pending.metadataVersion) {
				apply.Skipped++
				if _, duplicate := staleItems[pending.itemID]; !duplicate {
					staleItems[pending.itemID] = struct{}{}
					apply.Stale++
				}
				protectedFromMiss[pending.itemID] = struct{}{}
				continue
			}
			current, exists := shard.values[pending.itemID]
			if exists && current.MetadataVersion == pending.metadataVersion && !sourceIsNewer(pending.clock, pending.ns, current) {
				current.LastFetchAt = pending.fetchedAt
				shard.values[pending.itemID] = current
				outcomes[pending.itemID] = applyOutcome{itemID: pending.itemID, unchanged: true}
				continue
			}

			sourceTimestamp := time.Unix(pending.clock, pending.ns)
			state := ValueState{
				ItemID:          pending.itemID,
				Value:           pending.value,
				SourceTimestamp: sourceTimestamp,
				SourceNS:        pending.ns,
				LastFetchAt:     pending.fetchedAt,
				LastSuccessAt:   pending.fetchedAt,
				Delay:           pending.delay,
				ExpireAt:        ValueExpireAt(sourceTimestamp, pending.delay),
				MetadataVersion: pending.metadataVersion,
				ValueVersion:    c.nextValueVersion.Add(1),
				PublishSlot:     c.publishSlot(pending.itemID),
				PublishPolicy:   PublishPolicyForDelay(pending.delay),
				Valid:           true,
			}
			shard.insert(state)
			if registrar != nil {
				expiryEntries = append(expiryEntries, ExpiryEntry{
					ItemID:       state.ItemID,
					ValueVersion: state.ValueVersion,
					ExpireAt:     state.ExpireAt,
				})
			}
			if exists {
				outcomes[pending.itemID] = applyOutcome{itemID: pending.itemID, updated: true}
			} else {
				c.entries.Add(1)
				outcomes[pending.itemID] = applyOutcome{itemID: pending.itemID, created: true}
			}
		}
		shard.mu.Unlock()
	}
	if registrar != nil && len(expiryEntries) != 0 {
		registrar.RegisterExpiry(expiryEntries...)
	}

	for _, outcome := range outcomes {
		switch {
		case outcome.created:
			apply.Created++
		case outcome.updated:
			apply.Updated++
		case outcome.unchanged:
			apply.Unchanged++
		}
	}
	if apply.Stale != 0 {
		// A root batch issued from mixed metadata must be retried as one unit.
		// Current-definition values already applied above remain useful, but no
		// miss candidate may escape from this incomplete attempt.
		observeApplyResult(apply)
		metrics.ValueCacheEntries.Set(float64(c.Len()))
		return apply
	}

	missSet := make(map[string]struct{}, len(result.Query.ItemIDs))
	for _, itemID := range result.Query.ItemIDs {
		if _, protected := protectedFromMiss[itemID]; protected {
			continue
		}
		if _, returned := returnedItems[itemID]; !returned {
			missSet[itemID] = struct{}{}
		}
	}
	apply.MissItemIDs = make([]string, 0, len(missSet))
	for _, itemID := range result.Query.ItemIDs {
		if _, miss := missSet[itemID]; miss {
			apply.MissItemIDs = append(apply.MissItemIDs, itemID)
			delete(missSet, itemID)
		}
	}
	if len(missSet) != 0 {
		extra := make([]string, 0, len(missSet))
		for itemID := range missSet {
			extra = append(extra, itemID)
		}
		sort.Strings(extra)
		apply.MissItemIDs = append(apply.MissItemIDs, extra...)
	}
	observeApplyResult(apply)
	metrics.ValueCacheEntries.Set(float64(c.Len()))
	return apply
}

// metadataVersionCurrent is called while the target value shard is locked.
// If metadata publishes after this check, its subsequent reconciliation delete
// waits for the shard and removes the old-definition write. If it published
// before the check, the write is rejected here. This closes both orderings.
func (c *ValueCache) metadataVersionCurrent(itemID string, version uint64) bool {
	if c.metadataSource == nil {
		return true
	}
	snapshot := c.metadataSource.Snapshot()
	if snapshot == nil {
		return false
	}
	item := snapshot.Items[itemID]
	return item != nil && item.Version == version
}

func (c *ValueCache) MarkMiss(itemIDs []string, fetchedAt time.Time) MissResult {
	var result MissResult
	buckets, touched := c.groupItemIDs(itemIDs)
	for _, shardIndex := range touched {
		shard := &c.shards[shardIndex]
		shard.mu.Lock()
		for _, itemID := range buckets[shardIndex] {
			state, exists := shard.values[itemID]
			if !exists {
				result.NotFound++
				continue
			}
			if state.ConsecutiveMisses < math.MaxUint32 {
				state.ConsecutiveMisses++
			}
			state.LastFetchAt = fetchedAt
			if state.Valid && state.ConsecutiveMisses >= c.missThreshold && fetchedAt.After(missInvalidAfter(state.SourceTimestamp, state.Delay)) {
				state.Valid = false
				result.Invalidated++
			}
			shard.values[itemID] = state
			result.Marked++
		}
		shard.mu.Unlock()
	}
	metrics.ValueCacheMissesTotal.WithLabelValues("marked").Add(float64(result.Marked))
	metrics.ValueCacheMissesTotal.WithLabelValues("invalidated").Add(float64(result.Invalidated))
	metrics.ValueCacheMissesTotal.WithLabelValues("not_found").Add(float64(result.NotFound))
	return result
}

func (c *ValueCache) Delete(itemIDs []string, reason DeleteReason) DeleteResult {
	result := DeleteResult{Reason: reason}
	buckets, touched := c.groupItemIDs(itemIDs)
	for _, shardIndex := range touched {
		shard := &c.shards[shardIndex]
		shard.mu.Lock()
		for _, itemID := range buckets[shardIndex] {
			if shard.delete(itemID) {
				result.Deleted++
				c.entries.Add(-1)
			} else {
				result.NotFound++
			}
		}
		shard.compactOrder()
		shard.mu.Unlock()
	}
	observeDeleteResult(result)
	metrics.ValueCacheEntries.Set(float64(c.Len()))
	return result
}

func (c *ValueCache) DeleteVersioned(items []VersionedItem, reason DeleteReason) DeleteResult {
	result := DeleteResult{Reason: reason}
	buckets := make([][]VersionedItem, len(c.shards))
	touched := make([]int, 0)
	touchedSet := make([]bool, len(c.shards))
	for _, item := range items {
		shardIndex := c.shardIndex(item.ItemID)
		buckets[shardIndex] = append(buckets[shardIndex], item)
		if !touchedSet[shardIndex] {
			touchedSet[shardIndex] = true
			touched = append(touched, shardIndex)
		}
	}
	sort.Ints(touched)
	for _, shardIndex := range touched {
		shard := &c.shards[shardIndex]
		shard.mu.Lock()
		for _, item := range buckets[shardIndex] {
			state, exists := shard.values[item.ItemID]
			if !exists {
				result.NotFound++
				continue
			}
			if state.ValueVersion != item.ValueVersion {
				result.Stale++
				continue
			}
			shard.delete(item.ItemID)
			result.Deleted++
			c.entries.Add(-1)
		}
		shard.compactOrder()
		shard.mu.Unlock()
	}
	observeDeleteResult(result)
	metrics.ValueCacheEntries.Set(float64(c.Len()))
	return result
}

func (c *ValueCache) Load(itemID string) (ValueState, bool) {
	shard := &c.shards[c.shardIndex(itemID)]
	shard.mu.RLock()
	state, exists := shard.values[itemID]
	shard.mu.RUnlock()
	return state, exists
}

func (c *ValueCache) ReadPublishPage(slot uint16, cursor PublishCursor, cycleTimestamp time.Time, limit int) (PublishPage, error) {
	if slot >= c.publishSlots {
		return PublishPage{}, fmt.Errorf("publish slot %d out of range [0,%d)", slot, c.publishSlots)
	}
	if limit <= 0 {
		return PublishPage{}, fmt.Errorf("publish page limit must be positive: %d", limit)
	}
	if int(cursor.Shard) > len(c.shards) {
		return PublishPage{}, fmt.Errorf("publish cursor shard %d out of range", cursor.Shard)
	}
	if int(cursor.Shard) == len(c.shards) {
		return PublishPage{Next: cursor, Done: true}, nil
	}

	page := PublishPage{Values: make([]PublishValue, 0, limit), Next: cursor}
	for shardIndex := int(cursor.Shard); shardIndex < len(c.shards); shardIndex++ {
		sequence := uint64(0)
		if shardIndex == int(cursor.Shard) {
			sequence = cursor.Sequence
		}
		shard := &c.shards[shardIndex]
		shard.mu.RLock()
		start := shard.firstSequenceAfter(sequence)
		for i := start; i < len(shard.order); i++ {
			key := shard.order[i]
			page.Next = PublishCursor{Shard: uint16(shardIndex), Sequence: key.sequence}
			state, exists := shard.values[key.itemID]
			if !exists || state.scanSequence != key.sequence || state.PublishSlot != slot || !publishEligible(state, cycleTimestamp) {
				continue
			}
			timestamp := state.SourceTimestamp
			if state.PublishPolicy == PublishCycleTimestamp {
				timestamp = cycleTimestamp
			}
			page.Values = append(page.Values, PublishValue{
				ItemID:          state.ItemID,
				Value:           state.Value,
				Timestamp:       timestamp,
				SourceTimestamp: state.SourceTimestamp,
				SourceNS:        state.SourceNS,
				MetadataVersion: state.MetadataVersion,
				ValueVersion:    state.ValueVersion,
				PublishSlot:     state.PublishSlot,
				PublishPolicy:   state.PublishPolicy,
			})
			if len(page.Values) == limit {
				shard.mu.RUnlock()
				return page, nil
			}
		}
		shard.mu.RUnlock()
		page.Next = PublishCursor{Shard: uint16(shardIndex + 1)}
	}
	page.Done = true
	return page, nil
}

func publishEligible(state ValueState, cycleTimestamp time.Time) bool {
	if !state.Valid || !cycleTimestamp.Before(state.ExpireAt) {
		return false
	}
	if state.PublishPolicy == PublishSourceTimestamp {
		return state.ValueVersion > state.LastPublishedVersion
	}
	if cycleTimestamp.Before(state.SourceTimestamp) {
		return false
	}
	return state.LastPublishedTimestamp.IsZero() || cycleTimestamp.After(state.LastPublishedTimestamp)
}

// ReadScrapePage returns every currently valid state exactly once, independent
// of LastPublishedVersion/LastPublishedTimestamp. Pull is read-only and never
// acknowledges or otherwise mutates publishing state.
func (c *ValueCache) ReadScrapePage(cursor PublishCursor, scrapeTimestamp time.Time, limit int) (PublishPage, error) {
	if scrapeTimestamp.IsZero() {
		return PublishPage{}, fmt.Errorf("scrape timestamp must not be zero")
	}
	if limit <= 0 {
		return PublishPage{}, fmt.Errorf("scrape page limit must be positive: %d", limit)
	}
	if int(cursor.Shard) > len(c.shards) {
		return PublishPage{}, fmt.Errorf("scrape cursor shard %d out of range", cursor.Shard)
	}
	if int(cursor.Shard) == len(c.shards) {
		return PublishPage{Next: cursor, Done: true}, nil
	}

	page := PublishPage{Values: make([]PublishValue, 0, limit), Next: cursor}
	for shardIndex := int(cursor.Shard); shardIndex < len(c.shards); shardIndex++ {
		sequence := uint64(0)
		if shardIndex == int(cursor.Shard) {
			sequence = cursor.Sequence
		}
		shard := &c.shards[shardIndex]
		shard.mu.RLock()
		start := shard.firstSequenceAfter(sequence)
		for i := start; i < len(shard.order); i++ {
			key := shard.order[i]
			page.Next = PublishCursor{Shard: uint16(shardIndex), Sequence: key.sequence}
			state, exists := shard.values[key.itemID]
			if !exists || state.scanSequence != key.sequence || !scrapeEligible(state, scrapeTimestamp) {
				continue
			}
			timestamp := state.SourceTimestamp
			if state.PublishPolicy == PublishCycleTimestamp {
				timestamp = scrapeTimestamp
			}
			page.Values = append(page.Values, PublishValue{
				ItemID:          state.ItemID,
				Value:           state.Value,
				Timestamp:       timestamp,
				SourceTimestamp: state.SourceTimestamp,
				SourceNS:        state.SourceNS,
				MetadataVersion: state.MetadataVersion,
				ValueVersion:    state.ValueVersion,
				PublishSlot:     state.PublishSlot,
				PublishPolicy:   state.PublishPolicy,
			})
			if len(page.Values) == limit {
				shard.mu.RUnlock()
				return page, nil
			}
		}
		shard.mu.RUnlock()
		page.Next = PublishCursor{Shard: uint16(shardIndex + 1)}
	}
	page.Done = true
	return page, nil
}

func scrapeEligible(state ValueState, scrapeTimestamp time.Time) bool {
	if !state.Valid || !scrapeTimestamp.Before(state.ExpireAt) {
		return false
	}
	return state.PublishPolicy != PublishCycleTimestamp || !scrapeTimestamp.Before(state.SourceTimestamp)
}

func (c *ValueCache) AckPublished(acks []PublishAck) AckResult {
	var result AckResult
	buckets := make([][]PublishAck, len(c.shards))
	touched := make([]int, 0)
	touchedSet := make([]bool, len(c.shards))
	for _, ack := range acks {
		shardIndex := c.shardIndex(ack.ItemID)
		buckets[shardIndex] = append(buckets[shardIndex], ack)
		if !touchedSet[shardIndex] {
			touchedSet[shardIndex] = true
			touched = append(touched, shardIndex)
		}
	}
	sort.Ints(touched)
	for _, shardIndex := range touched {
		shard := &c.shards[shardIndex]
		shard.mu.Lock()
		for _, ack := range buckets[shardIndex] {
			state, exists := shard.values[ack.ItemID]
			if !exists {
				result.NotFound++
				continue
			}
			if state.ValueVersion != ack.ValueVersion {
				result.Stale++
				continue
			}
			changed := false
			if state.PublishPolicy == PublishSourceTimestamp {
				if state.LastPublishedVersion < ack.ValueVersion {
					state.LastPublishedVersion = ack.ValueVersion
					changed = true
				}
			} else if !ack.PublishedTimestamp.IsZero() && (state.LastPublishedTimestamp.IsZero() || ack.PublishedTimestamp.After(state.LastPublishedTimestamp)) {
				changed = true
			}
			if ack.PublishedTimestamp.After(state.LastPublishedTimestamp) {
				state.LastPublishedTimestamp = ack.PublishedTimestamp
			}
			if !changed {
				result.Stale++
				continue
			}
			shard.values[ack.ItemID] = state
			result.Applied++
		}
		shard.mu.Unlock()
	}
	return result
}

func (c *ValueCache) Len() int {
	return int(c.entries.Load())
}

// Stats scans one shard at a time so observability does not require per-item
// Prometheus labels. Callers should sample it at a low frequency.
func (c *ValueCache) Stats(now time.Time) ValueCacheStats {
	if now.IsZero() {
		now = time.Now()
	}
	var stats ValueCacheStats
	for i := range c.shards {
		shard := &c.shards[i]
		shard.mu.RLock()
		stats.Entries += len(shard.values)
		for _, state := range shard.values {
			if !state.Valid {
				continue
			}
			stats.Valid++
			if now.Before(state.ExpireAt) {
				stats.Fresh++
			}
		}
		shard.mu.RUnlock()
	}
	return stats
}

func (c *ValueCache) PublishSlotCount() uint16 {
	return c.publishSlots
}

func (c *ValueCache) shardIndex(itemID string) int {
	return int(SeriesFingerprint(itemID) & c.shardMask)
}

func (c *ValueCache) publishSlot(itemID string) uint16 {
	return uint16(SeriesFingerprint(itemID) % uint64(c.publishSlots))
}

func (c *ValueCache) groupItemIDs(itemIDs []string) ([][]string, []int) {
	buckets := make([][]string, len(c.shards))
	touched := make([]int, 0)
	touchedSet := make([]bool, len(c.shards))
	seen := make(map[string]struct{}, len(itemIDs))
	for _, itemID := range itemIDs {
		if _, duplicate := seen[itemID]; duplicate {
			continue
		}
		seen[itemID] = struct{}{}
		shardIndex := c.shardIndex(itemID)
		buckets[shardIndex] = append(buckets[shardIndex], itemID)
		if !touchedSet[shardIndex] {
			touchedSet[shardIndex] = true
			touched = append(touched, shardIndex)
		}
	}
	sort.Ints(touched)
	return buckets, touched
}

func observeApplyResult(result ApplyResult) {
	metrics.ValueCacheUpdatesTotal.WithLabelValues("created").Add(float64(result.Created))
	metrics.ValueCacheUpdatesTotal.WithLabelValues("updated").Add(float64(result.Updated))
	metrics.ValueCacheUpdatesTotal.WithLabelValues("unchanged").Add(float64(result.Unchanged))
	metrics.ValueCacheUpdatesTotal.WithLabelValues("rejected").Add(float64(result.Rejected))
	metrics.ValueCacheUpdatesTotal.WithLabelValues("skipped").Add(float64(result.Skipped))
}

func observeDeleteResult(result DeleteResult) {
	if result.Deleted == 0 {
		return
	}
	metrics.ValueCacheExpiredTotal.WithLabelValues(result.Reason.String()).Add(float64(result.Deleted))
}
