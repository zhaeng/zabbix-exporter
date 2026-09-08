package cache

import (
	"container/heap"
	"fmt"
	"sync"
	"time"
)

const (
	// DefaultExpiryMaxEntriesPerRun bounds catch-up work after a process pause or
	// forward wall-clock jump. Later ticks continue at the oldest pending bucket.
	DefaultExpiryMaxEntriesPerRun = 100_000
	expiryDeleteBatchSize         = 4_096
	expiryBucketSeconds           = int64(60)
)

// ExpiryEntry identifies one immutable ValueState generation. Replaced
// generations deliberately remain in the wheel and become stale at deletion.
type ExpiryEntry struct {
	ItemID       string
	ValueVersion uint64
	ExpireAt     time.Time
}

// ExpiryRegistrar is injected into ValueCache. ApplyBatch calls it only after a
// new ValueVersion has been installed; duplicate and older source points do not
// add wheel entries or extend expiry.
type ExpiryRegistrar interface {
	RegisterExpiry(entries ...ExpiryEntry)
}

// VersionedDeleter is the shard-aware cache primitive used by ExpiryWheel. The
// ValueVersion comparison and map deletion happen under the same shard lock.
type VersionedDeleter interface {
	DeleteVersioned(items []VersionedItem, reason DeleteReason) DeleteResult
}

type ExpiryWheelConfig struct {
	MaxEntriesPerRun int
}

// ExpiryResult is also the metrics hand-off for the future pipeline integration.
// CleanupLag has minute resolution and measures how long the oldest complete
// pending bucket has been overdue; it is zero while only the current/future
// minute remains.
type ExpiryResult struct {
	Processed           int
	CompletedBuckets    int
	Deleted             int
	Stale               int
	NotFound            int
	Limited             bool
	PendingEntries      int
	PendingBuckets      int
	CleanupLag          time.Duration
	LastProcessedBucket int64
}

type ExpiryStats struct {
	PendingEntries      int
	PendingBuckets      int
	CleanupLag          time.Duration
	LastProcessedBucket int64
}

type expiryBucket struct {
	entries []ExpiryEntry
}

// ExpiryWheel is a sparse minute wheel. The heap contains only non-empty minute
// keys, so catching up after a long pause does not iterate through empty minutes.
// runMu serializes cleanup rounds; mu only protects short bucket operations and
// is never held while shard deletion runs.
type ExpiryWheel struct {
	runMu sync.Mutex
	mu    sync.Mutex

	buckets             map[int64]*expiryBucket
	keys                expiryBucketHeap
	pendingEntries      int
	maxEntriesPerRun    int
	lastProcessedBucket int64
	hasProcessedBucket  bool
}

var _ ExpiryRegistrar = (*ExpiryWheel)(nil)

func NewExpiryWheel(config ExpiryWheelConfig) (*ExpiryWheel, error) {
	if config.MaxEntriesPerRun == 0 {
		config.MaxEntriesPerRun = DefaultExpiryMaxEntriesPerRun
	}
	if config.MaxEntriesPerRun < 0 {
		return nil, fmt.Errorf("expiry max entries per run must be positive: %d", config.MaxEntriesPerRun)
	}
	return &ExpiryWheel{
		buckets:          make(map[int64]*expiryBucket),
		maxEntriesPerRun: config.MaxEntriesPerRun,
	}, nil
}

func NewDefaultExpiryWheel() *ExpiryWheel {
	wheel, err := NewExpiryWheel(ExpiryWheelConfig{})
	if err != nil {
		panic(err)
	}
	return wheel
}

// RegisterExpiry appends every new generation to its ExpireAt minute. It does
// not search for or remove older generations, keeping update work O(1).
func (w *ExpiryWheel) RegisterExpiry(entries ...ExpiryEntry) {
	if len(entries) == 0 {
		return
	}
	w.mu.Lock()
	for _, entry := range entries {
		key := expiryBucketKey(entry.ExpireAt)
		bucket := w.buckets[key]
		if bucket == nil {
			bucket = &expiryBucket{}
			w.buckets[key] = bucket
			heap.Push(&w.keys, key)
		}
		bucket.entries = append(bucket.entries, entry)
		w.pendingEntries++
	}
	w.mu.Unlock()
}

// ExpireDue processes the current and missed minute buckets, but examines no
// more than MaxEntriesPerRun entries. Entries later in the current minute are
// retained, while entries at or before now are deleted by ValueVersion.
func (w *ExpiryWheel) ExpireDue(now time.Time, store VersionedDeleter) ExpiryResult {
	if store == nil {
		return w.resultAt(now)
	}

	w.runMu.Lock()
	defer w.runMu.Unlock()

	currentKey := expiryBucketKey(now)
	result := ExpiryResult{}
	var activeKey int64
	activeRemaining := 0
	hasActiveBucket := false

	for result.Processed < w.maxEntriesPerRun {
		w.mu.Lock()
		key, bucket, ok := w.firstDueBucketLocked(currentKey)
		if !ok {
			w.mu.Unlock()
			break
		}
		if !hasActiveBucket || activeKey != key {
			activeKey = key
			activeRemaining = len(bucket.entries)
			hasActiveBucket = true
		}
		if activeRemaining == 0 {
			w.removeEmptyBucketLocked(key, bucket)
			w.mu.Unlock()
			result.CompletedBuckets++
			hasActiveBucket = false
			continue
		}

		batchSize := activeRemaining
		if remainingBudget := w.maxEntriesPerRun - result.Processed; batchSize > remainingBudget {
			batchSize = remainingBudget
		}
		if batchSize > expiryDeleteBatchSize {
			batchSize = expiryDeleteBatchSize
		}
		if batchSize > len(bucket.entries) {
			batchSize = len(bucket.entries)
		}

		candidates := make([]VersionedItem, 0, batchSize)
		deferred := make([]ExpiryEntry, 0)
		for _, entry := range bucket.entries[:batchSize] {
			if entry.ExpireAt.After(now) {
				deferred = append(deferred, entry)
				continue
			}
			candidates = append(candidates, VersionedItem{
				ItemID:       entry.ItemID,
				ValueVersion: entry.ValueVersion,
			})
		}
		bucket.entries = bucket.entries[batchSize:]
		bucket.entries = append(bucket.entries, deferred...)
		w.pendingEntries -= len(candidates)
		activeRemaining -= batchSize
		result.Processed += batchSize

		bucketRemoved := len(bucket.entries) == 0
		if bucketRemoved {
			w.removeEmptyBucketLocked(key, bucket)
		}
		finishedInitialEntries := activeRemaining == 0
		w.mu.Unlock()

		if len(candidates) != 0 {
			deleted := store.DeleteVersioned(candidates, DeleteExpired)
			result.Deleted += deleted.Deleted
			result.Stale += deleted.Stale
			result.NotFound += deleted.NotFound
		}
		if bucketRemoved {
			result.CompletedBuckets++
			hasActiveBucket = false
			continue
		}
		// Deferred future entries and registrations concurrent with this round
		// are intentionally left for the next tick instead of being re-scanned.
		if finishedInitialEntries {
			break
		}
	}

	w.mu.Lock()
	result.Limited = result.Processed == w.maxEntriesPerRun && w.hasBucketAtOrBeforeLocked(currentKey)
	w.updateLastProcessedLocked(currentKey)
	w.fillResultLocked(now, &result)
	w.mu.Unlock()
	return result
}

func (w *ExpiryWheel) Stats(now time.Time) ExpiryStats {
	w.mu.Lock()
	stats := ExpiryStats{
		PendingEntries:      w.pendingEntries,
		PendingBuckets:      len(w.buckets),
		CleanupLag:          w.cleanupLagLocked(now),
		LastProcessedBucket: w.lastProcessedBucket,
	}
	w.mu.Unlock()
	return stats
}

func (w *ExpiryWheel) resultAt(now time.Time) ExpiryResult {
	w.mu.Lock()
	result := ExpiryResult{}
	w.fillResultLocked(now, &result)
	w.mu.Unlock()
	return result
}

func (w *ExpiryWheel) fillResultLocked(now time.Time, result *ExpiryResult) {
	result.PendingEntries = w.pendingEntries
	result.PendingBuckets = len(w.buckets)
	result.CleanupLag = w.cleanupLagLocked(now)
	result.LastProcessedBucket = w.lastProcessedBucket
}

func (w *ExpiryWheel) firstDueBucketLocked(currentKey int64) (int64, *expiryBucket, bool) {
	for len(w.keys) != 0 {
		key := w.keys[0]
		bucket := w.buckets[key]
		if bucket == nil || len(bucket.entries) == 0 {
			heap.Pop(&w.keys)
			delete(w.buckets, key)
			continue
		}
		if key > currentKey {
			return 0, nil, false
		}
		return key, bucket, true
	}
	return 0, nil, false
}

func (w *ExpiryWheel) removeEmptyBucketLocked(key int64, bucket *expiryBucket) {
	if current := w.buckets[key]; current != bucket || len(bucket.entries) != 0 {
		return
	}
	delete(w.buckets, key)
	if len(w.keys) != 0 && w.keys[0] == key {
		heap.Pop(&w.keys)
	}
}

func (w *ExpiryWheel) hasBucketAtOrBeforeLocked(currentKey int64) bool {
	_, _, ok := w.firstDueBucketLocked(currentKey)
	return ok
}

func (w *ExpiryWheel) updateLastProcessedLocked(currentKey int64) {
	processedThrough := currentKey
	if len(w.keys) != 0 && w.keys[0] <= currentKey {
		processedThrough = w.keys[0] - 1
	}
	if !w.hasProcessedBucket || processedThrough > w.lastProcessedBucket {
		w.lastProcessedBucket = processedThrough
		w.hasProcessedBucket = true
	}
}

func (w *ExpiryWheel) cleanupLagLocked(now time.Time) time.Duration {
	if len(w.keys) == 0 {
		return 0
	}
	oldestCompleteBucketEnd := time.Unix((w.keys[0]+1)*expiryBucketSeconds, 0)
	if !now.After(oldestCompleteBucketEnd) {
		return 0
	}
	return now.Sub(oldestCompleteBucketEnd)
}

func expiryBucketKey(at time.Time) int64 {
	return at.Unix() / expiryBucketSeconds
}

type expiryBucketHeap []int64

func (h expiryBucketHeap) Len() int           { return len(h) }
func (h expiryBucketHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h expiryBucketHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *expiryBucketHeap) Push(value any) {
	*h = append(*h, value.(int64))
}

func (h *expiryBucketHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}
