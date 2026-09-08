package collector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"zabbix-exporter/internal/cache"
	"zabbix-exporter/internal/metadata"
	"zabbix-exporter/internal/scheduler"
	"zabbix-exporter/internal/zabbix"
)

const (
	DefaultHistoryLimit  = 10_000
	DefaultMaxSplitDepth = 16
	DefaultOverlapMin    = 5 * time.Minute
	DefaultOverlapMax    = 15 * time.Minute
)

var (
	ErrPossiblyTruncated = errors.New("history batch remains possibly truncated")
	ErrMetadataChanged   = errors.New("metadata changed while history batch was in flight")
)

type HistoryConfig struct {
	Limit         int
	MaxSplitDepth int
	OverlapMin    time.Duration
	OverlapMax    time.Duration
	QueryTimeout  time.Duration
	// ReaderOwnsTimeout lets a runtime acquire a global concurrency slot before
	// starting the per-request timeout. Direct GroupCollector users retain the
	// original behavior when this is false.
	ReaderOwnsTimeout bool
}

type SnapshotSource interface {
	Snapshot() *metadata.MetadataSnapshot
}

type GroupCollector struct {
	reader   zabbix.HistoryReader
	values   cache.ValueStore
	metadata SnapshotSource
	config   HistoryConfig

	statesMu sync.Mutex
	states   map[string]*batchState
}

type batchState struct {
	mu            sync.Mutex
	lastQueryTill time.Time
}

type BatchCollectionResult struct {
	BatchID           string
	Complete          bool
	WatermarkBefore   time.Time
	WatermarkAfter    time.Time
	Queries           int
	Splits            int
	Applied           int
	Misses            int
	PossiblyTruncated bool
	Err               error
}

type GroupCollectionResult struct {
	GroupID string
	Batches []BatchCollectionResult
	Err     error
}

func NewGroupCollector(reader zabbix.HistoryReader, values cache.ValueStore, source SnapshotSource, config HistoryConfig) (*GroupCollector, error) {
	if reader == nil {
		return nil, errors.New("history reader is required")
	}
	if values == nil {
		return nil, errors.New("value store is required")
	}
	if source == nil {
		return nil, errors.New("metadata source is required")
	}
	if config.Limit == 0 {
		config.Limit = DefaultHistoryLimit
	}
	if config.MaxSplitDepth == 0 {
		config.MaxSplitDepth = DefaultMaxSplitDepth
	}
	if config.OverlapMin == 0 {
		config.OverlapMin = DefaultOverlapMin
	}
	if config.OverlapMax == 0 {
		config.OverlapMax = DefaultOverlapMax
	}
	if config.QueryTimeout == 0 {
		config.QueryTimeout = 30 * time.Second
	}
	if config.Limit < 1 {
		return nil, fmt.Errorf("history limit must be positive: %d", config.Limit)
	}
	if config.MaxSplitDepth < 1 {
		return nil, fmt.Errorf("max split depth must be positive: %d", config.MaxSplitDepth)
	}
	if config.OverlapMin < 0 || config.OverlapMax < config.OverlapMin {
		return nil, fmt.Errorf("invalid overlap range: min=%s max=%s", config.OverlapMin, config.OverlapMax)
	}
	if config.QueryTimeout <= 0 {
		return nil, fmt.Errorf("history query timeout must be positive: %s", config.QueryTimeout)
	}
	return &GroupCollector{
		reader:   reader,
		values:   values,
		metadata: source,
		config:   config,
		states:   make(map[string]*batchState),
	}, nil
}

func (c *GroupCollector) Execute(ctx context.Context, group scheduler.Group, queryTill time.Time) error {
	return c.CollectGroup(ctx, group, queryTill).Err
}

func (c *GroupCollector) CollectGroup(ctx context.Context, group scheduler.Group, queryTill time.Time) GroupCollectionResult {
	result := GroupCollectionResult{GroupID: group.ID}
	if queryTill.IsZero() {
		queryTill = time.Now()
	}
	for _, batch := range group.Batches {
		if err := ctx.Err(); err != nil {
			result.Err = errors.Join(result.Err, err)
			break
		}
		batchResult := c.collectBatch(ctx, group, batch, queryTill)
		result.Batches = append(result.Batches, batchResult)
		result.Err = errors.Join(result.Err, batchResult.Err)
	}
	return result
}

func (c *GroupCollector) LastQueryTill(batchID string) time.Time {
	c.statesMu.Lock()
	state := c.states[batchID]
	c.statesMu.Unlock()
	if state == nil {
		return time.Time{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.lastQueryTill
}

// RetainGroups bounds watermark state across complete metadata replacements.
func (c *GroupCollector) RetainGroups(groups []scheduler.Group) {
	keep := make(map[string]struct{})
	for _, group := range groups {
		for _, batch := range group.Batches {
			keep[batch.ID] = struct{}{}
		}
	}
	c.statesMu.Lock()
	for batchID := range c.states {
		if _, ok := keep[batchID]; !ok {
			delete(c.states, batchID)
		}
	}
	c.statesMu.Unlock()
}

func (c *GroupCollector) collectBatch(ctx context.Context, group scheduler.Group, batch scheduler.Batch, queryTill time.Time) BatchCollectionResult {
	result := BatchCollectionResult{BatchID: batch.ID}
	state := c.state(batch.ID)
	state.mu.Lock()
	defer state.mu.Unlock()

	result.WatermarkBefore = state.lastQueryTill
	if !state.lastQueryTill.IsZero() && queryTill.Before(state.lastQueryTill) {
		result.WatermarkAfter = state.lastQueryTill
		result.Err = fmt.Errorf("query till %s is before batch watermark %s", queryTill, state.lastQueryTill)
		return result
	}

	timeFrom := queryTill.Add(-bootstrapWindow(group.Delay))
	if !state.lastQueryTill.IsZero() {
		timeFrom = state.lastQueryTill.Add(-overlapForDelay(group.Delay, c.config.OverlapMin, c.config.OverlapMax))
	}
	query := zabbix.HistoryQuery{
		GroupID:   group.ID,
		ItemIDs:   append([]string(nil), batch.ItemIDs...),
		ValueType: group.ValueType,
		TimeFrom:  timeFrom,
		TimeTill:  queryTill,
	}
	snapshot := c.metadata.Snapshot()
	attempt := c.query(ctx, query, group.Delay, snapshot, 0)
	result.Queries = attempt.queries
	result.Splits = attempt.splits
	result.Applied = attempt.applied
	result.PossiblyTruncated = attempt.truncated
	result.Err = attempt.err
	if !attempt.complete {
		result.WatermarkAfter = state.lastQueryTill
		return result
	}

	misses := uniqueSorted(attempt.misses)
	if len(misses) > 0 {
		fetchedAt := attempt.finishedAt
		if fetchedAt.IsZero() {
			fetchedAt = queryTill
		}
		c.values.MarkMiss(misses, fetchedAt)
	}
	state.lastQueryTill = queryTill
	result.Complete = true
	result.Misses = len(misses)
	result.WatermarkAfter = state.lastQueryTill
	return result
}

type queryResult struct {
	complete   bool
	queries    int
	splits     int
	applied    int
	misses     []string
	finishedAt time.Time
	truncated  bool
	err        error
}

func (c *GroupCollector) query(
	ctx context.Context,
	query zabbix.HistoryQuery,
	delay time.Duration,
	snapshot *metadata.MetadataSnapshot,
	depth int,
) queryResult {
	if err := ctx.Err(); err != nil {
		return queryResult{err: err}
	}
	query.ItemIDs = append([]string(nil), query.ItemIDs...)
	query.Limit = c.queryLimit(query, delay)
	queryCtx := ctx
	cancel := func() {}
	if !c.config.ReaderOwnsTimeout {
		queryCtx, cancel = context.WithTimeout(ctx, c.config.QueryTimeout)
	}
	batch := c.reader.QueryHistoryBatch(queryCtx, query)
	cancel()
	batch.Query = query
	result := queryResult{queries: 1, finishedAt: batch.FinishedAt}

	truncated := batch.Status == zabbix.BatchPossiblyTruncated || batch.LimitHit ||
		query.Limit > 0 && batch.Returned >= query.Limit
	if truncated {
		result.truncated = true
		if len(query.ItemIDs) < 2 || depth >= c.config.MaxSplitDepth {
			result.err = fmt.Errorf("%w: group=%s items=%d depth=%d", ErrPossiblyTruncated, query.GroupID, len(query.ItemIDs), depth)
			return result
		}
		middle := len(query.ItemIDs) / 2
		leftQuery := query
		leftQuery.ItemIDs = append([]string(nil), query.ItemIDs[:middle]...)
		rightQuery := query
		rightQuery.ItemIDs = append([]string(nil), query.ItemIDs[middle:]...)
		left := c.query(ctx, leftQuery, delay, snapshot, depth+1)
		right := c.query(ctx, rightQuery, delay, snapshot, depth+1)
		return mergeSplit(left, right)
	}

	if batch.Status != zabbix.BatchSuccess && batch.Status != zabbix.BatchEmpty {
		if batch.Err != nil {
			result.err = batch.Err
		} else {
			result.err = fmt.Errorf("history query returned %s", batch.Status)
		}
		return result
	}
	batch = filterExpiredLatest(batch, delay, query.TimeTill)
	apply := c.values.ApplyBatch(batch, snapshot)
	if apply.Stale != 0 {
		result.err = fmt.Errorf("%w: group=%s stale_items=%d", ErrMetadataChanged, query.GroupID, apply.Stale)
		return result
	}
	result.complete = true
	result.applied = apply.Created + apply.Updated + apply.Unchanged
	result.misses = append(result.misses, apply.MissItemIDs...)
	return result
}

func filterExpiredLatest(batch zabbix.BatchResult, delay time.Duration, queryTill time.Time) zabbix.BatchResult {
	latest := batch.Latest
	if latest == nil {
		latest = zabbix.ReduceLatest(batch.Records)
	}
	filtered := make(map[string]zabbix.HistoryItem, len(latest))
	for itemID, record := range latest {
		ns := int64(record.NS)
		if ns < 0 || ns >= int64(time.Second) {
			filtered[itemID] = record
			continue
		}
		sourceTimestamp := time.Unix(int64(record.Clock), ns)
		if cache.ValueExpireAt(sourceTimestamp, delay).After(queryTill) {
			filtered[itemID] = record
		}
	}
	batch.Latest = filtered
	return batch
}

func mergeSplit(left, right queryResult) queryResult {
	result := queryResult{
		complete:   left.complete && right.complete,
		queries:    1 + left.queries + right.queries,
		splits:     1 + left.splits + right.splits,
		applied:    left.applied + right.applied,
		misses:     append(append([]string(nil), left.misses...), right.misses...),
		finishedAt: left.finishedAt,
		truncated:  true,
		err:        errors.Join(left.err, right.err),
	}
	if right.finishedAt.After(result.finishedAt) {
		result.finishedAt = right.finishedAt
	}
	return result
}

func (c *GroupCollector) queryLimit(query zabbix.HistoryQuery, delay time.Duration) int {
	if delay <= 0 || len(query.ItemIDs) == 0 {
		return c.config.Limit
	}
	window := query.TimeTill.Sub(query.TimeFrom)
	if window < 0 {
		window = 0
	}
	expectedPerItem := int64(window / delay)
	if window%delay != 0 {
		expectedPerItem++
	}
	expectedPerItem += 2
	itemCount := int64(len(query.ItemIDs))
	if expectedPerItem >= int64(c.config.Limit) || expectedPerItem > int64(c.config.Limit)/itemCount {
		return c.config.Limit
	}
	calculated := expectedPerItem * itemCount
	if calculated < 1 {
		calculated = 1
	}
	if calculated > int64(c.config.Limit) {
		return c.config.Limit
	}
	return int(calculated)
}

func (c *GroupCollector) state(batchID string) *batchState {
	c.statesMu.Lock()
	defer c.statesMu.Unlock()
	state := c.states[batchID]
	if state == nil {
		state = &batchState{}
		c.states[batchID] = state
	}
	return state
}

func overlapForDelay(delay, minimum, maximum time.Duration) time.Duration {
	overlap := delay / 5
	if overlap < minimum {
		overlap = minimum
	}
	if maximum > 0 && overlap > maximum {
		overlap = maximum
	}
	return overlap
}

func bootstrapWindow(delay time.Duration) time.Duration {
	if delay <= time.Minute {
		window := 2 * delay
		if window < 5*time.Minute {
			window = 5 * time.Minute
		}
		return window
	}
	grace := delay / 5
	if grace < 30*time.Second {
		grace = 30 * time.Second
	}
	return 2*delay + grace
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
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

var _ scheduler.GroupExecutor = (*GroupCollector)(nil)
