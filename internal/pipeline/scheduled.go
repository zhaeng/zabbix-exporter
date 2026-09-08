package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"zabbix-exporter/internal/cache"
	"zabbix-exporter/internal/collector"
	"zabbix-exporter/internal/log"
	"zabbix-exporter/internal/metadata"
	"zabbix-exporter/internal/metrics"
	"zabbix-exporter/internal/promwrap"
	"zabbix-exporter/internal/scheduler"
	"zabbix-exporter/internal/zabbix"
)

const defaultShutdownTimeout = 5 * time.Second

type LifecycleObserver func(component, action string)

type ScheduledConfig struct {
	Metadata           metadata.RefresherConfig
	Scheduler          scheduler.Config
	History            collector.HistoryConfig
	HistoryConcurrency int
	ValueCache         cache.ValueCacheConfig
	Expiry             cache.ExpiryWheelConfig
	ExpiryTick         time.Duration
	Publisher          *promwrap.StreamingPublisherConfig
	PublishInterval    time.Duration
	ShutdownTimeout    time.Duration
	Observer           LifecycleObserver
}

func (c *ScheduledConfig) setDefaults() {
	if c.Metadata.RefreshInterval <= 0 {
		c.Metadata.RefreshInterval = metadata.DefaultRefresherConfig().RefreshInterval
	}
	if c.HistoryConcurrency == 0 {
		c.HistoryConcurrency = 4
	}
	if c.History.QueryTimeout == 0 {
		c.History.QueryTimeout = 30 * time.Second
	}
	if c.ExpiryTick == 0 {
		c.ExpiryTick = time.Minute
	}
	if c.PublishInterval == 0 {
		c.PublishInterval = time.Minute
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
}

func (c ScheduledConfig) validate() error {
	if c.ExpiryTick <= 0 {
		return fmt.Errorf("expiry tick must be positive: %s", c.ExpiryTick)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown timeout must be positive: %s", c.ShutdownTimeout)
	}
	if c.HistoryConcurrency < 1 {
		return fmt.Errorf("history concurrency must be positive: %d", c.HistoryConcurrency)
	}
	if c.Publisher != nil {
		if c.PublishInterval <= 0 {
			return fmt.Errorf("publish interval must be positive: %s", c.PublishInterval)
		}
		if c.Publisher.SpreadSlots == 0 {
			return fmt.Errorf("publisher spread slots must be positive")
		}
		if c.PublishInterval/time.Duration(c.Publisher.SpreadSlots) <= 0 {
			return fmt.Errorf("publish interval %s is too small for %d slots", c.PublishInterval, c.Publisher.SpreadSlots)
		}
	}
	return nil
}

// Scheduled owns exactly one metadata→scheduler→expiry→publisher pipeline.
// Its single ownership graph makes accidental double collection or double
// publishing impossible inside the runtime.
type Scheduled struct {
	config    ScheduledConfig
	store     *metadata.Store
	refresher *metadata.Refresher
	values    *cache.ValueCache
	wheel     *cache.ExpiryWheel
	collector *collector.GroupCollector
	scheduler *scheduler.Scheduler
	publisher *promwrap.Publisher

	stateMu    sync.Mutex
	starting   bool
	started    bool
	stopping   bool
	startDone  chan struct{}
	stopDone   chan struct{}
	stopErr    error
	rootCancel context.CancelFunc

	metadataCancel context.CancelFunc
	metadataDone   chan struct{}
	expiryCancel   context.CancelFunc
	expiryDone     chan struct{}
	publishCancel  context.CancelFunc
	publishDone    chan struct{}
}

func NewScheduled(cfg ScheduledConfig, metadataReader metadata.Reader, historyReader zabbix.HistoryReader) (*Scheduled, error) {
	if metadataReader == nil {
		return nil, fmt.Errorf("metadata reader is required")
	}
	if historyReader == nil {
		return nil, fmt.Errorf("history reader is required")
	}
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	store := metadata.NewStore()
	wheel, err := cache.NewExpiryWheel(cfg.Expiry)
	if err != nil {
		return nil, err
	}
	valueConfig := cfg.ValueCache
	valueConfig.ExpiryRegistrar = wheel
	valueConfig.MetadataSource = store
	values, err := cache.NewValueCache(valueConfig)
	if err != nil {
		return nil, err
	}
	refresher := metadata.NewRefresher(metadataReader, store, cfg.Metadata)
	historyConfig := cfg.History
	historyConfig.ReaderOwnsTimeout = true
	groupCollector, err := collector.NewGroupCollector(&observedHistoryReader{
		next:    historyReader,
		sem:     make(chan struct{}, cfg.HistoryConcurrency),
		timeout: cfg.History.QueryTimeout,
	}, values, store, historyConfig)
	if err != nil {
		return nil, err
	}
	groupScheduler, err := scheduler.NewScheduler(store, groupCollector, cfg.Scheduler)
	if err != nil {
		return nil, err
	}
	var publisher *promwrap.Publisher
	if cfg.Publisher != nil {
		publisher, err = promwrap.NewStreamingPublisher(*cfg.Publisher, values, store)
		if err != nil {
			return nil, err
		}
	}
	return &Scheduled{
		config: cfg, store: store, refresher: refresher, values: values, wheel: wheel,
		collector: groupCollector, scheduler: groupScheduler, publisher: publisher,
	}, nil
}

func (p *Scheduled) Start(parent context.Context) (startErr error) {
	if parent == nil {
		return fmt.Errorf("scheduled pipeline context is required")
	}
	p.stateMu.Lock()
	if p.started {
		p.stateMu.Unlock()
		return nil
	}
	if p.starting {
		p.stateMu.Unlock()
		return errors.New("scheduled pipeline is already starting")
	}
	if p.stopping {
		p.stateMu.Unlock()
		return errors.New("scheduled pipeline cannot restart after stop")
	}
	rootCtx, rootCancel := context.WithCancel(parent)
	startDone := make(chan struct{})
	p.starting = true
	p.startDone = startDone
	p.rootCancel = rootCancel
	p.stateMu.Unlock()
	defer func() {
		p.stateMu.Lock()
		p.starting = false
		if startErr == nil && !p.stopping {
			p.started = true
		}
		close(startDone)
		p.stateMu.Unlock()
	}()

	metadataCtx, metadataCancel := context.WithCancel(rootCtx)
	result, err := p.refresh(metadataCtx)
	if err != nil {
		metadataCancel()
		rootCancel()
		return fmt.Errorf("initial metadata refresh: %w", err)
	}
	p.applyMetadata(result)
	p.metadataCancel = metadataCancel
	p.metadataDone = make(chan struct{})
	go p.runMetadata(metadataCtx, p.metadataDone)
	p.observe("metadata", "started")
	if err := rootCtx.Err(); err != nil {
		return p.abortStart(fmt.Errorf("start scheduled pipeline: %w", err))
	}

	p.scheduler.Start(rootCtx)
	p.observe("scheduler", "started")
	if err := rootCtx.Err(); err != nil {
		return p.abortStart(fmt.Errorf("start scheduled pipeline: %w", err))
	}

	expiryCtx, expiryCancel := context.WithCancel(rootCtx)
	p.expiryCancel = expiryCancel
	p.expiryDone = make(chan struct{})
	go p.runExpiry(expiryCtx, p.expiryDone)
	p.observe("expiry", "started")
	if err := rootCtx.Err(); err != nil {
		return p.abortStart(fmt.Errorf("start scheduled pipeline: %w", err))
	}

	if p.publisher != nil {
		if err := p.publisher.Start(rootCtx); err != nil {
			return p.abortStart(fmt.Errorf("start streaming publisher: %w", err))
		}
		publishCtx, publishCancel := context.WithCancel(rootCtx)
		p.publishCancel = publishCancel
		p.publishDone = make(chan struct{})
		go p.runPublisher(publishCtx, p.publishDone)
		p.observe("publisher", "started")
	}
	if err := rootCtx.Err(); err != nil {
		return p.abortStart(fmt.Errorf("start scheduled pipeline: %w", err))
	}
	return nil
}

func (p *Scheduled) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.stateMu.Lock()
	if !p.stopping {
		p.stopping = true
		p.stopDone = make(chan struct{})
		startDone := p.startDone
		stopDone := p.stopDone
		if p.starting && p.rootCancel != nil {
			// A normal, fully-started stop drains in reverse order. During a
			// synchronous first refresh there is nothing to drain yet, so cancel
			// immediately to make startup itself interruptible.
			p.rootCancel()
		}
		go p.finishStop(startDone, stopDone)
	}
	done := p.stopDone
	p.stateMu.Unlock()

	boundedCtx, cancel := context.WithTimeout(ctx, p.config.ShutdownTimeout)
	defer cancel()
	select {
	case <-done:
		p.stateMu.Lock()
		err := p.stopErr
		p.stateMu.Unlock()
		return err
	case <-boundedCtx.Done():
		return boundedCtx.Err()
	}
}

// abortStart arranges the same single-owner cleanup used by Stop. The cleanup
// waits for Start's deferred startDone close, so partially initialized fields
// are never raced by a second stopper.
func (p *Scheduled) abortStart(err error) error {
	p.stateMu.Lock()
	if !p.stopping {
		p.stopping = true
		p.stopDone = make(chan struct{})
		if p.rootCancel != nil {
			p.rootCancel()
		}
		go p.finishStop(p.startDone, p.stopDone)
	}
	p.stateMu.Unlock()
	return err
}

func (p *Scheduled) finishStop(startDone, stopDone chan struct{}) {
	if startDone != nil {
		<-startDone
	}
	err := p.stopStarted()
	if p.rootCancel != nil {
		p.rootCancel()
	}
	p.stateMu.Lock()
	p.started = false
	p.stopErr = err
	close(stopDone)
	p.stateMu.Unlock()
}

// stopStarted has one background owner and does not pretend completion when a
// caller's Stop deadline expires. The publisher receives a bounded graceful
// drain, then every component is canceled and joined in strict reverse order.
func (p *Scheduled) stopStarted() error {
	var stopErr error
	if p.publishCancel != nil {
		p.publishCancel()
		stopErr = errors.Join(stopErr, waitDone(context.Background(), p.publishDone))
	}
	if p.publisher != nil && p.publishDone != nil {
		drainCtx, cancel := context.WithTimeout(context.Background(), p.config.ShutdownTimeout)
		stopErr = errors.Join(stopErr, p.publisher.WaitIdle(drainCtx))
		cancel()
		stopErr = errors.Join(stopErr, p.publisher.StopContext(context.Background()))
		p.observe("publisher", "stopped")
	}

	if p.expiryCancel != nil {
		p.expiryCancel()
		stopErr = errors.Join(stopErr, waitDone(context.Background(), p.expiryDone))
		p.observe("expiry", "stopped")
	}

	if p.scheduler.State().Started {
		stopErr = errors.Join(stopErr, p.scheduler.StopContext(context.Background()))
		p.observe("scheduler", "stopped")
	}

	if p.metadataCancel != nil {
		p.metadataCancel()
		stopErr = errors.Join(stopErr, waitDone(context.Background(), p.metadataDone))
		p.observe("metadata", "stopped")
	}
	return stopErr
}

func (p *Scheduled) Ready() error {
	p.stateMu.Lock()
	started := p.started
	stopping := p.stopping
	p.stateMu.Unlock()
	if !started || stopping {
		return errors.New("scheduled pipeline is not running")
	}
	snapshot := p.store.Snapshot()
	if snapshot == nil || snapshot.Version == 0 || snapshot.CreatedAt.IsZero() {
		return errors.New("metadata snapshot is not ready")
	}
	state := p.scheduler.State()
	if !state.Started || !state.Running {
		return errors.New("scheduler is not running")
	}
	p.observeMetadataAge(time.Now())
	return nil
}

func (p *Scheduled) Store() *metadata.Store          { return p.store }
func (p *Scheduled) Values() *cache.ValueCache       { return p.values }
func (p *Scheduled) Scheduler() *scheduler.Scheduler { return p.scheduler }
func (p *Scheduled) Publisher() *promwrap.Publisher  { return p.publisher }

func (p *Scheduled) runMetadata(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(p.config.Metadata.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := p.refresh(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Warn("metadata refresh failed: %v", err)
				}
				continue
			}
			p.applyMetadata(result)
		}
	}
}

func (p *Scheduled) refresh(ctx context.Context) (metadata.RefreshResult, error) {
	started := time.Now()
	result, err := p.refresher.Refresh(ctx)
	status := "success"
	if err != nil {
		status = "error"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = "canceled"
		}
	}
	metrics.MetadataRefreshTotal.WithLabelValues(status).Inc()
	metrics.MetadataRefreshDuration.Observe(time.Since(started).Seconds())
	if err == nil {
		metrics.MetadataHosts.Set(float64(result.HostCount))
		metrics.MetadataItems.Set(float64(result.ItemCount))
		metrics.MetadataEnabledItems.Set(float64(result.EnabledItemCount))
	}
	return result, err
}

func (p *Scheduled) applyMetadata(result metadata.RefreshResult) {
	snapshot := p.store.Snapshot()
	if len(result.Diff.RemovedItems) != 0 {
		p.values.Delete(result.Diff.RemovedItems, cache.DeleteMetadataRemoved)
	}
	if len(result.Diff.ChangedItems) != 0 {
		changedEnabled := make([]string, 0, len(result.Diff.ChangedItems))
		for _, itemID := range result.Diff.ChangedItems {
			item := snapshot.Items[itemID]
			// Disabled items cannot receive new values. Keep any old ValueCache
			// state until its source-derived hard TTL expires.
			if item != nil && !item.Enabled {
				continue
			}
			changedEnabled = append(changedEnabled, itemID)
		}
		if len(changedEnabled) != 0 {
			p.values.Delete(changedEnabled, cache.DeleteMetadataChanged)
		}
	}
	if p.scheduler.State().Started {
		p.scheduler.ApplyMetadataDiff(result.Diff)
	}
	p.observeMetadataAge(time.Now())
}

func (p *Scheduled) runExpiry(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(p.config.ExpiryTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			result := p.wheel.ExpireDue(now, p.values)
			metrics.ExpiryBucketEntries.Set(float64(result.PendingEntries))
			metrics.ExpiryPendingBuckets.Set(float64(result.PendingBuckets))
			metrics.ExpiryLag.Set(result.CleanupLag.Seconds())
			if result.Limited {
				metrics.ExpiryLimitedTotal.Inc()
			}
			cacheStats := p.values.Stats(now)
			metrics.ValueCacheEntries.Set(float64(cacheStats.Entries))
			metrics.ValueCacheValidEntries.Set(float64(cacheStats.Valid))
			metrics.ValueCacheFreshEntries.Set(float64(cacheStats.Fresh))
			p.observeMetadataAge(now)
		}
	}
}

func (p *Scheduled) runPublisher(ctx context.Context, done chan struct{}) {
	defer close(done)
	slots := p.config.Publisher.SpreadSlots
	tick := p.config.PublishInterval / time.Duration(slots)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	var slot uint16
	for {
		started := time.Now()
		_, err := p.publisher.PublishSlot(ctx, slot, started)
		metrics.PushSlotDuration.Observe(time.Since(started).Seconds())
		metrics.PushQueueEntries.Set(float64(p.publisher.QueueDepth()))
		if err != nil && ctx.Err() == nil && !errors.Is(err, promwrap.ErrPushQueueFull) {
			log.Warn("streaming publish slot failed (slot=%d): %v", slot, err)
		}
		slot = (slot + 1) % slots
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Scheduled) observeMetadataAge(now time.Time) {
	snapshot := p.store.Snapshot()
	if snapshot == nil || snapshot.CreatedAt.IsZero() {
		metrics.MetadataAge.Set(0)
		return
	}
	age := now.Sub(snapshot.CreatedAt)
	if age < 0 {
		age = 0
	}
	metrics.MetadataAge.Set(age.Seconds())
}

func (p *Scheduled) observe(component, action string) {
	if p.config.Observer != nil {
		p.config.Observer(component, action)
	}
}

func waitDone(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type observedHistoryReader struct {
	next    zabbix.HistoryReader
	sem     chan struct{}
	timeout time.Duration
}

func (r *observedHistoryReader) QueryHistoryBatch(ctx context.Context, query zabbix.HistoryQuery) zabbix.BatchResult {
	started := time.Now()
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		result := zabbix.BatchResult{
			Query: query, Status: zabbix.BatchAPIError, Err: ctx.Err(),
			StartedAt: started, FinishedAt: time.Now(),
		}
		metrics.HistoryBatchesTotal.WithLabelValues(historyMetricStatus(result, ctx.Err())).Inc()
		metrics.HistoryResponseRecords.Observe(0)
		metrics.HistoryBatchDuration.Observe(time.Since(started).Seconds())
		return result
	}
	queryCtx, cancel := context.WithTimeout(ctx, r.timeout)
	result := r.next.QueryHistoryBatch(queryCtx, query)
	requestErr := queryCtx.Err()
	cancel()
	status := historyMetricStatus(result, requestErr)
	metrics.HistoryBatchesTotal.WithLabelValues(status).Inc()
	metrics.HistoryResponseRecords.Observe(float64(result.Returned))
	metrics.HistoryBatchDuration.Observe(time.Since(started).Seconds())
	if result.LimitHit {
		metrics.HistoryLimitHitsTotal.Inc()
	}
	return result
}

// historyMetricStatus returns only a fixed, low-cardinality status set. The
// request context state must be sampled before the observer's own cancel call;
// otherwise every normally completed request would be mislabeled canceled.
func historyMetricStatus(result zabbix.BatchResult, requestErr error) string {
	if errors.Is(requestErr, context.DeadlineExceeded) || errors.Is(result.Err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(requestErr, context.Canceled) || errors.Is(result.Err, context.Canceled) {
		return "canceled"
	}
	switch result.Status {
	case zabbix.BatchSuccess, zabbix.BatchEmpty, zabbix.BatchPossiblyTruncated, zabbix.BatchAPIError, zabbix.BatchDecodeError:
		return result.Status.String()
	default:
		return "api_error"
	}
}
