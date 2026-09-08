package metadata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/zhaeng/zabbix-exporter/internal/config"
	"github.com/zhaeng/zabbix-exporter/internal/log"
	"github.com/zhaeng/zabbix-exporter/internal/zabbix"
)

// RefresherConfig is intentionally independent from history collection.
type RefresherConfig struct {
	RefreshInterval    time.Duration
	RequestTimeout     time.Duration
	BatchHosts         int
	Workers            int
	MinRequestInterval time.Duration
	DelayFallback      time.Duration
	Labels             config.LabelsConfig
}

func DefaultRefresherConfig() RefresherConfig {
	return RefresherConfig{
		RefreshInterval:    20 * time.Minute,
		RequestTimeout:     30 * time.Second,
		BatchHosts:         50,
		Workers:            2,
		MinRequestInterval: 100 * time.Millisecond,
		DelayFallback:      time.Minute,
	}
}

// RefresherConfigFromApp maps the frozen T03 app configuration without wiring
// the refresher into the server entrypoint.
func RefresherConfigFromApp(app *config.Config) RefresherConfig {
	defaults := DefaultRefresherConfig()
	if app == nil {
		return defaults
	}
	return RefresherConfig{
		RefreshInterval:    app.Collector.MetadataRefreshInterval,
		RequestTimeout:     app.Collector.MetadataRequestTimeout,
		BatchHosts:         app.Collector.MetadataBatchHosts,
		Workers:            app.Collector.MetadataWorkers,
		MinRequestInterval: app.Collector.MetadataMinRequestInterval,
		DelayFallback:      app.Collector.MetadataDelayFallback,
		Labels:             app.Labels,
	}.withDefaults()
}

func (cfg RefresherConfig) withDefaults() RefresherConfig {
	defaults := DefaultRefresherConfig()
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaults.RefreshInterval
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaults.RequestTimeout
	}
	if cfg.BatchHosts <= 0 {
		cfg.BatchHosts = defaults.BatchHosts
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaults.Workers
	}
	// A negative interval explicitly disables pacing in deterministic tests.
	if cfg.MinRequestInterval < 0 {
		cfg.MinRequestInterval = 0
	} else if cfg.MinRequestInterval == 0 {
		cfg.MinRequestInterval = defaults.MinRequestInterval
	}
	if cfg.DelayFallback <= 0 {
		cfg.DelayFallback = defaults.DelayFallback
	}
	return cfg
}

// RefreshResult is emitted only for one attempted complete refresh. Diff is
// populated only when every required request and snapshot validation succeeded.
type RefreshResult struct {
	StartedAt        time.Time
	CompletedAt      time.Time
	SnapshotVersion  uint64
	HostCount        int
	ItemCount        int
	EnabledItemCount int
	GroupCount       int
	ItemBatches      int
	SucceededBatches int
	DelayFallbacks   map[DelayParseStatus]int
	Diff             MetadataDiff
}

type Refresher struct {
	reader  Reader
	store   MetadataStore
	config  RefresherConfig
	builder *Builder
	limiter *requestLimiter

	refreshMu sync.Mutex
	now       func() time.Time
}

func NewRefresher(reader Reader, store MetadataStore, cfg RefresherConfig) *Refresher {
	cfg = cfg.withDefaults()
	return &Refresher{
		reader:  reader,
		store:   store,
		config:  cfg,
		builder: NewBuilder(cfg.Labels, cfg.DelayFallback),
		limiter: newRequestLimiter(cfg.MinRequestInterval),
		now:     time.Now,
	}
}

// Refresh builds a complete candidate outside Store and applies it once. Any
// host request, item batch, validation, timeout, or cancellation error leaves
// the currently published snapshot and its version unchanged.
func (r *Refresher) Refresh(ctx context.Context) (RefreshResult, error) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	result := RefreshResult{
		StartedAt:      r.now(),
		DelayFallbacks: make(map[DelayParseStatus]int),
	}
	if r.reader == nil || r.store == nil {
		result.CompletedAt = r.now()
		return result, errors.New("metadata refresher requires a reader and store")
	}

	hosts, err := r.getHosts(ctx)
	if err != nil {
		result.CompletedAt = r.now()
		return result, fmt.Errorf("refresh host metadata: %w", err)
	}
	result.HostCount = len(hosts)
	hostIDs := make([]string, 0, len(hosts))
	for _, host := range hosts {
		hostIDs = append(hostIDs, host.HostID)
	}
	sort.Strings(hostIDs)
	batches := splitStrings(hostIDs, r.config.BatchHosts)
	result.ItemBatches = len(batches)

	items, succeeded, err := r.getItemBatches(ctx, batches)
	result.SucceededBatches = succeeded
	if err != nil {
		result.CompletedAt = r.now()
		return result, err
	}
	result.ItemCount = len(items)

	candidate, stats, err := r.builder.Build(hosts, items, r.now())
	result.DelayFallbacks = stats.DelayFallbacks
	if err != nil {
		result.CompletedAt = r.now()
		return result, fmt.Errorf("build complete metadata snapshot: %w", err)
	}
	for _, item := range candidate.Items {
		if item != nil && item.Enabled {
			result.EnabledItemCount++
		}
	}
	result.GroupCount = len(candidate.Groups)
	for status, count := range stats.DelayFallbacks {
		log.Warn("metadata delay fallback (status=%s, count=%d, fallback=%s)", status, count, r.config.DelayFallback)
	}

	result.Diff = r.store.ApplySnapshot(candidate)
	result.SnapshotVersion = result.Diff.NewVersion
	result.CompletedAt = r.now()
	return result, nil
}

// Run performs one startup refresh and then low-frequency periodic refreshes.
// Failed attempts are logged and the last complete snapshot remains available.
func (r *Refresher) Run(ctx context.Context) error {
	r.refreshAndLog(ctx)
	ticker := time.NewTicker(r.config.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.refreshAndLog(ctx)
		}
	}
}

func (r *Refresher) refreshAndLog(ctx context.Context) {
	result, err := r.Refresh(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("metadata refresh failed (batches=%d, succeeded=%d): %v", result.ItemBatches, result.SucceededBatches, err)
		}
		return
	}
	log.Info("metadata refresh complete (version=%d, hosts=%d, items=%d, groups=%d, batches=%d)",
		result.SnapshotVersion, result.HostCount, result.ItemCount, result.GroupCount, result.ItemBatches)
}

func (r *Refresher) getHosts(ctx context.Context) ([]zabbix.Host, error) {
	if err := r.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, r.config.RequestTimeout)
	defer cancel()
	return r.reader.GetHosts(requestCtx)
}

type itemBatchResult struct {
	index int
	items []zabbix.Item
	err   error
}

func (r *Refresher) getItemBatches(ctx context.Context, batches [][]string) ([]zabbix.Item, int, error) {
	if len(batches) == 0 {
		return []zabbix.Item{}, 0, nil
	}
	workerCount := r.config.Workers
	if workerCount > len(batches) {
		workerCount = len(batches)
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(batches))
	results := make(chan itemBatchResult, len(batches))
	for index := range batches {
		jobs <- index
	}
	close(jobs)

	var workers sync.WaitGroup
	workers.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if workCtx.Err() != nil {
					results <- itemBatchResult{index: index, err: workCtx.Err()}
					continue
				}
				items, err := r.getItemBatch(workCtx, batches[index])
				results <- itemBatchResult{index: index, items: items, err: err}
				if err != nil {
					cancel()
				}
			}
		}()
	}

	byBatch := make([][]zabbix.Item, len(batches))
	errorsByBatch := make([]error, len(batches))
	succeeded := 0
	for range batches {
		batchResult := <-results
		if batchResult.err != nil {
			errorsByBatch[batchResult.index] = batchResult.err
			continue
		}
		if err := validateBatchItems(batches[batchResult.index], batchResult.items); err != nil {
			errorsByBatch[batchResult.index] = err
			cancel()
			continue
		}
		succeeded++
		byBatch[batchResult.index] = batchResult.items
	}
	workers.Wait()

	for index, batchErr := range errorsByBatch {
		if batchErr != nil {
			return nil, succeeded, fmt.Errorf("refresh item metadata batch %d/%d: %w", index+1, len(batches), batchErr)
		}
	}
	items := make([]zabbix.Item, 0)
	for _, batchItems := range byBatch {
		items = append(items, batchItems...)
	}
	return items, succeeded, nil
}

func (r *Refresher) getItemBatch(ctx context.Context, hostIDs []string) ([]zabbix.Item, error) {
	if err := r.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, r.config.RequestTimeout)
	defer cancel()
	return r.reader.GetItemMetadata(requestCtx, append([]string(nil), hostIDs...))
}

func validateBatchItems(hostIDs []string, items []zabbix.Item) error {
	allowed := make(map[string]struct{}, len(hostIDs))
	for _, hostID := range hostIDs {
		allowed[hostID] = struct{}{}
	}
	for _, item := range items {
		if _, ok := allowed[item.HostID]; !ok {
			return fmt.Errorf("item %q returned for host %q outside requested batch", item.ItemID, item.HostID)
		}
	}
	return nil
}

func splitStrings(values []string, size int) [][]string {
	if len(values) == 0 {
		return nil
	}
	if size <= 0 {
		size = len(values)
	}
	batches := make([][]string, 0, (len(values)+size-1)/size)
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		batches = append(batches, append([]string(nil), values[start:end]...))
	}
	return batches
}

// requestLimiter provides metadata-only pacing. It is intentionally not shared
// with history workers, so a metadata burst cannot consume history capacity.
type requestLimiter struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

func newRequestLimiter(interval time.Duration) *requestLimiter {
	return &requestLimiter{interval: interval}
}

func (l *requestLimiter) Wait(ctx context.Context) error {
	if l.interval <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := time.Now()
	reserved := now
	if l.next.After(reserved) {
		reserved = l.next
	}
	l.next = reserved.Add(l.interval)
	l.mu.Unlock()

	wait := time.Until(reserved)
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
