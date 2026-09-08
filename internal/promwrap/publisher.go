package promwrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"zabbix-exporter/internal/cache"
	"zabbix-exporter/internal/metadata"
	"zabbix-exporter/internal/metrics"
)

const (
	defaultStreamingSpreadSlots   = 12
	defaultStreamingPageSize      = 5000
	defaultStreamingMaxSamples    = 5000
	defaultStreamingMaxBatchBytes = 4 << 20
	defaultStreamingWorkers       = 4
	defaultStreamingQueueCapacity = 100
	defaultStreamingMaxRetries    = 3
)

var (
	ErrPublisherNotStarted = errors.New("streaming publisher is not started")
	ErrPublisherStopped    = errors.New("streaming publisher is stopped")
	ErrPushQueueFull       = errors.New("remote write queue is full")
)

// PublishValueStore is the narrow T07 view of ValueCache. It deliberately
// excludes collection and expiry operations.
type PublishValueStore interface {
	ReadPublishPage(slot uint16, cursor cache.PublishCursor, cycleTimestamp time.Time, limit int) (cache.PublishPage, error)
	AckPublished(acks []cache.PublishAck) cache.AckResult
}

type MetadataSnapshotProvider interface {
	Snapshot() *metadata.MetadataSnapshot
}

type publishSlotCounter interface {
	PublishSlotCount() uint16
}

// StreamingPublisherConfig describes exactly one Remote Write endpoint. T07
// intentionally has no endpoint slice, HA ownership, or stale-marker setting.
type StreamingPublisherConfig struct {
	EndpointURL     string
	Timeout         time.Duration
	Username        string
	Password        string
	SpreadSlots     uint16
	PageSize        int
	MaxSamples      int
	MaxBatchBytes   int
	Workers         int
	QueueCapacity   int
	MaxRetries      int
	RetryBackoff    time.Duration
	MaxRetryBackoff time.Duration
	HTTPClient      *http.Client
}

func (c *StreamingPublisherConfig) setDefaults() {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.SpreadSlots == 0 {
		c.SpreadSlots = defaultStreamingSpreadSlots
	}
	if c.PageSize == 0 {
		c.PageSize = defaultStreamingPageSize
	}
	if c.MaxSamples == 0 {
		c.MaxSamples = defaultStreamingMaxSamples
	}
	if c.MaxBatchBytes == 0 {
		c.MaxBatchBytes = defaultStreamingMaxBatchBytes
	}
	if c.Workers == 0 {
		c.Workers = defaultStreamingWorkers
	}
	if c.QueueCapacity == 0 {
		c.QueueCapacity = defaultStreamingQueueCapacity
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = defaultStreamingMaxRetries
	}
	if c.RetryBackoff == 0 {
		c.RetryBackoff = 100 * time.Millisecond
	}
	if c.MaxRetryBackoff == 0 {
		c.MaxRetryBackoff = 2 * time.Second
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
}

func (c StreamingPublisherConfig) validate() error {
	endpoint := singleEndpoint{url: c.EndpointURL, timeout: c.Timeout, username: c.Username, password: c.Password}
	if err := endpoint.validate(); err != nil {
		return err
	}
	if c.SpreadSlots == 0 {
		return fmt.Errorf("spread slots must be positive")
	}
	if c.PageSize <= 0 {
		return fmt.Errorf("publish page size must be positive: %d", c.PageSize)
	}
	if c.MaxSamples <= 0 {
		return fmt.Errorf("max samples per send must be positive: %d", c.MaxSamples)
	}
	if c.MaxBatchBytes <= 0 {
		return fmt.Errorf("max batch bytes must be positive: %d", c.MaxBatchBytes)
	}
	if c.Workers <= 0 {
		return fmt.Errorf("push workers must be positive: %d", c.Workers)
	}
	if c.QueueCapacity <= 0 {
		return fmt.Errorf("push queue capacity must be positive: %d", c.QueueCapacity)
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("max retries must not be negative: %d", c.MaxRetries)
	}
	if c.RetryBackoff < 0 || c.MaxRetryBackoff <= 0 || c.RetryBackoff > c.MaxRetryBackoff {
		return fmt.Errorf("invalid retry backoff range: %s..%s", c.RetryBackoff, c.MaxRetryBackoff)
	}
	return nil
}

type PublishSlotResult struct {
	Slot             uint16
	CycleTimestamp   time.Time
	HostPagesRead    int
	HostSeriesRead   int
	PagesRead        int
	ValuesRead       int
	BatchesQueued    int
	SamplesQueued    int
	SkippedMetadata  int
	AlreadyPending   int
	OversizedSamples int
	CoalescedBatches int
	CoalescedSamples int
}

type PublisherStats struct {
	QueuedBatches     uint64
	QueuedSamples     uint64
	UncompressedBytes uint64
	CompressedBytes   uint64
	SentBatches       uint64
	SentSamples       uint64
	FailedBatches     uint64
	RetryAttempts     uint64
	PermanentFailures uint64
	RetryableFailures uint64
	CoalescedBatches  uint64
	CoalescedSamples  uint64
	QueueFull         uint64
	MaxQueuedBatches  uint64
	AckApplied        uint64
	AckStale          uint64
	AckNotFound       uint64
}

type publisherCounters struct {
	queuedBatches     atomic.Uint64
	queuedSamples     atomic.Uint64
	uncompressedBytes atomic.Uint64
	compressedBytes   atomic.Uint64
	sentBatches       atomic.Uint64
	sentSamples       atomic.Uint64
	failedBatches     atomic.Uint64
	retryAttempts     atomic.Uint64
	permanentFailures atomic.Uint64
	retryableFailures atomic.Uint64
	coalescedBatches  atomic.Uint64
	coalescedSamples  atomic.Uint64
	queueFull         atomic.Uint64
	maxQueuedBatches  atomic.Uint64
	ackApplied        atomic.Uint64
	ackStale          atomic.Uint64
	ackNotFound       atomic.Uint64
}

// Publisher reads ValueCache pages, encodes bounded Remote Write requests, and
// hands them to one bounded single-endpoint worker pool. It is not wired into
// main until T08.
type Publisher struct {
	config   StreamingPublisherConfig
	values   PublishValueStore
	metadata MetadataSnapshotProvider
	encoder  batchEncoder
	queue    *laneQueue
	worker   pushWorker

	stateMu  sync.Mutex
	started  bool
	stopped  bool
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopDone chan struct{}

	slotMu        []sync.Mutex
	lastRequested []time.Time
	lastCycle     []time.Time

	reservationMu sync.Mutex
	reservations  map[publishKey]struct{}
	activity      publishActivity
	counters      publisherCounters
}

func NewStreamingPublisher(config StreamingPublisherConfig, values PublishValueStore, metadataProvider MetadataSnapshotProvider) (*Publisher, error) {
	if values == nil {
		return nil, fmt.Errorf("publish value store is required")
	}
	if metadataProvider == nil {
		return nil, fmt.Errorf("metadata provider is required")
	}
	config.setDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	if counter, ok := values.(publishSlotCounter); ok && counter.PublishSlotCount() != config.SpreadSlots {
		return nil, fmt.Errorf("publisher spread slots %d do not match ValueCache slots %d", config.SpreadSlots, counter.PublishSlotCount())
	}
	encoder, err := newBatchEncoder(config.MaxSamples, config.MaxBatchBytes)
	if err != nil {
		return nil, err
	}
	endpoint := singleEndpoint{
		url:      config.EndpointURL,
		timeout:  config.Timeout,
		username: config.Username,
		password: config.Password,
	}
	return &Publisher{
		config:        config,
		values:        values,
		metadata:      metadataProvider,
		encoder:       encoder,
		queue:         newLaneQueue(config.Workers, config.QueueCapacity),
		worker:        pushWorker{endpoint: endpoint, client: config.HTTPClient, maxRetries: config.MaxRetries, backoff: config.RetryBackoff, maxBackoff: config.MaxRetryBackoff},
		slotMu:        make([]sync.Mutex, int(config.SpreadSlots)),
		lastRequested: make([]time.Time, int(config.SpreadSlots)),
		lastCycle:     make([]time.Time, int(config.SpreadSlots)),
		reservations:  make(map[publishKey]struct{}),
		activity:      newPublishActivity(),
	}, nil
}

func (p *Publisher) Start(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("publisher context is required")
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.stopped {
		return ErrPublisherStopped
	}
	if p.started {
		return nil
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.started = true
	p.stopDone = make(chan struct{})
	stopDone := p.stopDone
	for lane := 0; lane < p.config.Workers; lane++ {
		p.wg.Add(1)
		go p.runWorker(lane)
	}
	go func() {
		p.wg.Wait()
		close(stopDone)
	}()
	return nil
}

func (p *Publisher) Stop() {
	_ = p.StopContext(context.Background())
}

// StopContext prevents new queue admission, cancels workers, releases queued
// reservations, and waits only until the supplied deadline.
func (p *Publisher) StopContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.stateMu.Lock()
	if p.stopped {
		done := p.stopDone
		p.stateMu.Unlock()
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
	p.stopped = true
	if p.cancel != nil {
		p.cancel()
	}
	remaining := p.queue.close()
	done := p.stopDone
	p.stateMu.Unlock()

	for _, batch := range remaining {
		p.releaseBatch(batch)
		p.activity.done()
	}
	metrics.PushQueueEntries.Set(0)
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

func (p *Publisher) PublishSlot(ctx context.Context, slot uint16, requestedCycle time.Time) (PublishSlotResult, error) {
	if slot >= p.config.SpreadSlots {
		return PublishSlotResult{}, fmt.Errorf("publish slot %d out of range [0,%d)", slot, p.config.SpreadSlots)
	}
	if requestedCycle.IsZero() {
		return PublishSlotResult{}, fmt.Errorf("cycle timestamp must not be zero")
	}
	if err := p.ready(); err != nil {
		return PublishSlotResult{}, err
	}

	p.slotMu[int(slot)].Lock()
	defer p.slotMu[int(slot)].Unlock()
	cycle := p.normalizeCycleLocked(slot, requestedCycle)
	result := PublishSlotResult{Slot: slot, CycleTimestamp: cycle}

	current, removed := p.queue.beginLongCycle(slot, cycle.UnixMilli())
	if !current {
		return result, nil
	}
	for _, batch := range removed {
		result.CoalescedBatches++
		result.CoalescedSamples += batch.samples
		p.recordCoalesced(batch)
		p.releaseBatch(batch)
		p.activity.done()
	}
	if len(removed) != 0 {
		metrics.PushCoalescedCyclesTotal.Inc()
	}

	snapshot := p.metadata.Snapshot()
	if snapshot == nil {
		return result, fmt.Errorf("metadata snapshot is nil")
	}
	var oversizedErr error
	hostResult, hostErr := p.processHosts(ctx, snapshot, slot, cycle)
	result.HostPagesRead += hostResult.PagesRead
	result.HostSeriesRead += hostResult.SeriesRead
	result.BatchesQueued += hostResult.BatchesQueued
	result.SamplesQueued += hostResult.SamplesQueued
	result.SkippedMetadata += hostResult.SkippedMetadata
	result.AlreadyPending += hostResult.AlreadyPending
	result.OversizedSamples += hostResult.OversizedSamples
	oversizedErr = hostResult.oversizedErr
	if hostErr != nil {
		return result, errors.Join(oversizedErr, hostErr)
	}

	var cursor cache.PublishCursor
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		page, err := p.values.ReadPublishPage(slot, cursor, cycle, p.config.PageSize)
		if err != nil {
			return result, err
		}
		result.PagesRead++
		result.ValuesRead += len(page.Values)
		pageResult, pageErr := p.processPage(snapshot, slot, cycle, page.Values)
		result.BatchesQueued += pageResult.BatchesQueued
		result.SamplesQueued += pageResult.SamplesQueued
		result.SkippedMetadata += pageResult.SkippedMetadata
		result.AlreadyPending += pageResult.AlreadyPending
		result.OversizedSamples += pageResult.OversizedSamples
		if oversizedErr == nil {
			oversizedErr = pageResult.oversizedErr
		}
		if pageErr != nil {
			return result, errors.Join(oversizedErr, pageErr)
		}
		if page.Done {
			return result, oversizedErr
		}
		if page.Next == cursor {
			return result, fmt.Errorf("publish page cursor did not advance: %#v", cursor)
		}
		cursor = page.Next
	}
}

func (p *Publisher) ready() error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.stopped {
		return ErrPublisherStopped
	}
	if !p.started {
		return ErrPublisherNotStarted
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (p *Publisher) normalizeCycleLocked(slot uint16, requested time.Time) time.Time {
	requested = requested.UTC().Truncate(time.Millisecond)
	index := int(slot)
	if p.lastRequested[index].Equal(requested) {
		return p.lastCycle[index]
	}
	cycle := requested
	if !p.lastCycle[index].IsZero() && !cycle.After(p.lastCycle[index]) {
		cycle = p.lastCycle[index].Add(time.Millisecond)
	}
	p.lastRequested[index] = requested
	p.lastCycle[index] = cycle
	return cycle
}

type pageProcessResult struct {
	BatchesQueued    int
	SamplesQueued    int
	SkippedMetadata  int
	AlreadyPending   int
	OversizedSamples int
	oversizedErr     error
}

func (p *Publisher) processPage(snapshot *metadata.MetadataSnapshot, slot uint16, cycle time.Time, values []cache.PublishValue) (pageProcessResult, error) {
	batcher := newPageBatcher(p, slot, cycle)
	for _, value := range values {
		item, exists := snapshot.Items[value.ItemID]
		if !exists || item == nil || item.Version != value.MetadataVersion || len(item.Labels) == 0 {
			batcher.result.SkippedMetadata++
			continue
		}
		if value.PublishPolicy != cache.PublishSourceTimestamp && value.PublishPolicy != cache.PublishCycleTimestamp {
			batcher.result.SkippedMetadata++
			continue
		}

		key := publishKey{seriesID: "item\x00" + value.ItemID, generation: value.ValueVersion, timestampMS: value.Timestamp.UnixMilli()}
		lane := workerLaneForSeries(value.ItemID, p.config.Workers)
		ack := cache.PublishAck{ItemID: value.ItemID, ValueVersion: value.ValueVersion, PublishedTimestamp: value.Timestamp}
		sample := encodeSeries{
			seriesID:  value.ItemID,
			labels:    item.Labels,
			value:     value.Value,
			timestamp: value.Timestamp,
			ack:       &ack,
			key:       key,
		}
		if err := batcher.add(value.PublishPolicy, lane, sample); err != nil {
			return batcher.result, err
		}
	}
	return batcher.finish()
}

type hostProcessResult struct {
	pageProcessResult
	PagesRead  int
	SeriesRead int
}

func (p *Publisher) processHosts(ctx context.Context, snapshot *metadata.MetadataSnapshot, slot uint16, cycle time.Time) (hostProcessResult, error) {
	var result hostProcessResult
	batcher := newPageBatcher(p, slot, cycle)
	pageEntries := 0

	finishPage := func() error {
		if pageEntries == 0 {
			return nil
		}
		pageResult, err := batcher.finish()
		mergePageProcessResult(&result.pageProcessResult, pageResult)
		result.PagesRead++
		pageEntries = 0
		batcher = newPageBatcher(p, slot, cycle)
		return err
	}

	for _, hostID := range snapshot.HostIDs {
		if err := ctx.Err(); err != nil {
			batcher.cleanup()
			mergePageProcessResult(&result.pageProcessResult, batcher.result)
			return result, err
		}
		fingerprint := metadata.HostSeriesFingerprint(hostID)
		if uint16(fingerprint%uint64(p.config.SpreadSlots)) != slot {
			continue
		}
		series, ok := metadata.ExportableHostSeries(hostID, snapshot.Hosts[hostID])
		if !ok {
			result.SkippedMetadata++
			continue
		}
		result.SeriesRead++
		pageEntries++
		key := publishKey{seriesID: series.Key, generation: series.Version, timestampMS: cycle.UnixMilli()}
		sample := encodeSeries{
			seriesID:  series.Key,
			labels:    series.Labels,
			value:     1,
			timestamp: cycle,
			key:       key,
		}
		lane := int(series.Fingerprint % uint64(p.config.Workers))
		if err := batcher.add(cache.PublishCycleTimestamp, lane, sample); err != nil {
			mergePageProcessResult(&result.pageProcessResult, batcher.result)
			return result, err
		}
		if pageEntries == p.config.PageSize {
			if err := finishPage(); err != nil {
				return result, err
			}
		}
	}
	if err := finishPage(); err != nil {
		return result, err
	}
	return result, nil
}

func mergePageProcessResult(dst *pageProcessResult, source pageProcessResult) {
	dst.BatchesQueued += source.BatchesQueued
	dst.SamplesQueued += source.SamplesQueued
	dst.SkippedMetadata += source.SkippedMetadata
	dst.AlreadyPending += source.AlreadyPending
	dst.OversizedSamples += source.OversizedSamples
	if dst.oversizedErr == nil {
		dst.oversizedErr = source.oversizedErr
	}
}

type pageBatcher struct {
	p        *Publisher
	slot     uint16
	cycle    time.Time
	builders []*batchBuilder
	result   pageProcessResult
}

func newPageBatcher(p *Publisher, slot uint16, cycle time.Time) *pageBatcher {
	return &pageBatcher{
		p:        p,
		slot:     slot,
		cycle:    cycle,
		builders: make([]*batchBuilder, p.config.Workers*2),
	}
}

func (b *pageBatcher) add(policy cache.PublishPolicy, lane int, sample encodeSeries) error {
	if !b.p.reserve(sample.key) {
		b.result.AlreadyPending++
		return nil
	}
	index := lane*2 + int(policy)
	builder := b.builders[index]
	if builder == nil {
		builder = b.p.encoder.newBuilder(policy, b.slot, b.cycle.UnixMilli(), lane)
		b.builders[index] = builder
	}
	added, err := builder.add(sample)
	if err != nil {
		b.p.releaseKeys([]publishKey{sample.key})
		b.result.OversizedSamples++
		if b.result.oversizedErr == nil {
			b.result.oversizedErr = err
		}
		return nil
	}
	if added {
		return nil
	}

	batch, err := builder.encode()
	if err != nil {
		b.p.releaseKeys([]publishKey{sample.key})
		b.cleanup()
		return err
	}
	b.builders[index] = nil
	if err := b.p.submitBatch(batch); err != nil {
		b.p.releaseKeys([]publishKey{sample.key})
		b.cleanup()
		return err
	}
	b.result.BatchesQueued++
	b.result.SamplesQueued += batch.samples

	builder = b.p.encoder.newBuilder(policy, b.slot, b.cycle.UnixMilli(), lane)
	b.builders[index] = builder
	added, err = builder.add(sample)
	if err != nil || !added {
		b.p.releaseKeys([]publishKey{sample.key})
		b.cleanup()
		if err == nil {
			err = fmt.Errorf("series %q could not be added to an empty batch", sample.seriesID)
		}
		return err
	}
	return nil
}

func (b *pageBatcher) finish() (pageProcessResult, error) {
	for lane := 0; lane < b.p.config.Workers; lane++ {
		for policy := cache.PublishSourceTimestamp; policy <= cache.PublishCycleTimestamp; policy++ {
			index := lane*2 + int(policy)
			builder := b.builders[index]
			if builder == nil || builder.empty() {
				continue
			}
			batch, err := builder.encode()
			if err != nil {
				b.cleanup()
				return b.result, err
			}
			b.builders[index] = nil
			if err := b.p.submitBatch(batch); err != nil {
				b.cleanup()
				return b.result, err
			}
			b.result.BatchesQueued++
			b.result.SamplesQueued += batch.samples
		}
	}
	return b.result, nil
}

func (b *pageBatcher) cleanup() {
	for index, builder := range b.builders {
		if builder != nil {
			b.p.releaseKeys(builder.keys)
			b.builders[index] = nil
		}
	}
}

func (p *Publisher) submitBatch(batch *encodedBatch) error {
	p.activity.add()
	switch p.queue.tryEnqueue(batch) {
	case queueAccepted:
		p.counters.queuedBatches.Add(1)
		p.counters.queuedSamples.Add(uint64(batch.samples))
		p.counters.uncompressedBytes.Add(uint64(batch.uncompressedBytes))
		p.counters.compressedBytes.Add(uint64(batch.compressedBytes))
		p.recordMaxQueue(uint64(p.queue.len()))
		metrics.PushQueueEntries.Set(float64(p.queue.len()))
		metrics.PushBatchSeries.Observe(float64(batch.samples))
		metrics.PushBatchBytes.WithLabelValues("raw").Observe(float64(batch.uncompressedBytes))
		metrics.PushBatchBytes.WithLabelValues("snappy").Observe(float64(batch.compressedBytes))
		return nil
	case queueObsolete:
		metrics.PushBatchesTotal.WithLabelValues("stopped").Inc()
		p.releaseBatch(batch)
		p.activity.done()
		return ErrPublisherStopped
	case queueFull:
		p.counters.queueFull.Add(1)
		metrics.PushBatchesTotal.WithLabelValues("queue_full").Inc()
		p.releaseBatch(batch)
		p.activity.done()
		return ErrPushQueueFull
	default:
		panic("unknown queue admission")
	}
}

func (p *Publisher) runWorker(lane int) {
	defer p.wg.Done()
	for {
		batch, ok := p.queue.pop(p.ctx, lane)
		if !ok {
			return
		}
		metrics.PushQueueEntries.Set(float64(p.queue.len()))
		delivery := p.worker.send(p.ctx, batch)
		if delivery.attempts > 1 {
			p.counters.retryAttempts.Add(uint64(delivery.attempts - 1))
		}
		if delivery.err == nil {
			if len(batch.acks) != 0 {
				ack := p.values.AckPublished(batch.acks)
				p.counters.ackApplied.Add(uint64(ack.Applied))
				p.counters.ackStale.Add(uint64(ack.Stale))
				p.counters.ackNotFound.Add(uint64(ack.NotFound))
			}
			p.counters.sentBatches.Add(1)
			p.counters.sentSamples.Add(uint64(batch.samples))
			metrics.PushBatchesTotal.WithLabelValues("success").Inc()
		} else {
			p.counters.failedBatches.Add(1)
			if delivery.class == DeliveryPermanent {
				p.counters.permanentFailures.Add(1)
				metrics.PushBatchesTotal.WithLabelValues("permanent_failure").Inc()
			} else {
				p.counters.retryableFailures.Add(1)
				metrics.PushBatchesTotal.WithLabelValues("retryable_failure").Inc()
			}
		}
		p.releaseBatch(batch)
		p.activity.done()
	}
}

func (p *Publisher) reserve(key publishKey) bool {
	p.reservationMu.Lock()
	defer p.reservationMu.Unlock()
	if _, exists := p.reservations[key]; exists {
		return false
	}
	p.reservations[key] = struct{}{}
	return true
}

func (p *Publisher) releaseBatch(batch *encodedBatch) {
	p.releaseKeys(batch.keys)
}

func (p *Publisher) releaseKeys(keys []publishKey) {
	p.reservationMu.Lock()
	for _, key := range keys {
		delete(p.reservations, key)
	}
	p.reservationMu.Unlock()
}

func (p *Publisher) recordCoalesced(batch *encodedBatch) {
	p.counters.coalescedBatches.Add(1)
	p.counters.coalescedSamples.Add(uint64(batch.samples))
	metrics.PushBatchesTotal.WithLabelValues("coalesced").Inc()
}

func (p *Publisher) recordMaxQueue(size uint64) {
	for {
		current := p.counters.maxQueuedBatches.Load()
		if size <= current || p.counters.maxQueuedBatches.CompareAndSwap(current, size) {
			return
		}
	}
}

func (p *Publisher) WaitIdle(ctx context.Context) error {
	return p.activity.wait(ctx)
}

func (p *Publisher) QueueDepth() int {
	return p.queue.len()
}

func (p *Publisher) Stats() PublisherStats {
	return PublisherStats{
		QueuedBatches:     p.counters.queuedBatches.Load(),
		QueuedSamples:     p.counters.queuedSamples.Load(),
		UncompressedBytes: p.counters.uncompressedBytes.Load(),
		CompressedBytes:   p.counters.compressedBytes.Load(),
		SentBatches:       p.counters.sentBatches.Load(),
		SentSamples:       p.counters.sentSamples.Load(),
		FailedBatches:     p.counters.failedBatches.Load(),
		RetryAttempts:     p.counters.retryAttempts.Load(),
		PermanentFailures: p.counters.permanentFailures.Load(),
		RetryableFailures: p.counters.retryableFailures.Load(),
		CoalescedBatches:  p.counters.coalescedBatches.Load(),
		CoalescedSamples:  p.counters.coalescedSamples.Load(),
		QueueFull:         p.counters.queueFull.Load(),
		MaxQueuedBatches:  p.counters.maxQueuedBatches.Load(),
		AckApplied:        p.counters.ackApplied.Load(),
		AckStale:          p.counters.ackStale.Load(),
		AckNotFound:       p.counters.ackNotFound.Load(),
	}
}

type publishActivity struct {
	mu      sync.Mutex
	pending int
	idle    chan struct{}
}

func newPublishActivity() publishActivity {
	idle := make(chan struct{})
	close(idle)
	return publishActivity{idle: idle}
}

func (a *publishActivity) add() {
	a.mu.Lock()
	if a.pending == 0 {
		a.idle = make(chan struct{})
	}
	a.pending++
	a.mu.Unlock()
}

func (a *publishActivity) done() {
	a.mu.Lock()
	a.pending--
	if a.pending < 0 {
		a.mu.Unlock()
		panic("negative publisher activity")
	}
	if a.pending == 0 {
		close(a.idle)
	}
	a.mu.Unlock()
}

func (a *publishActivity) wait(ctx context.Context) error {
	a.mu.Lock()
	idle := a.idle
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-idle:
		return nil
	}
}
