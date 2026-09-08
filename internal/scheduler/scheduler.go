package scheduler

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/zhaeng/zabbix-exporter/internal/metadata"
	"github.com/zhaeng/zabbix-exporter/internal/metrics"
)

const (
	DefaultWorkerCount     = 4
	DefaultQueueCapacity   = 128
	DefaultRetryBase       = 5 * time.Second
	DefaultRetryMax        = 5 * time.Minute
	DefaultBootstrapSpread = time.Minute
)

type Config struct {
	BatchSize       int
	WorkerCount     int
	QueueCapacity   int
	RetryBase       time.Duration
	RetryMax        time.Duration
	BootstrapSpread time.Duration
}

type MetadataSource interface {
	Snapshot() *metadata.MetadataSnapshot
}

type GroupExecutor interface {
	Execute(ctx context.Context, group Group, queryTill time.Time) error
}

type batchRetainer interface {
	RetainGroups(groups []Group)
}

type Option func(*Scheduler)

func WithClock(clock Clock) Option {
	return func(s *Scheduler) {
		if clock != nil {
			s.clock = clock
		}
	}
}

type Scheduler struct {
	config   Config
	source   MetadataSource
	executor GroupExecutor
	builder  *GroupBuilder
	clock    Clock

	mu          sync.Mutex
	entries     map[string]*groupEntry
	queue       groupHeap
	inFlightIDs map[string]int

	tasks chan collectionTask
	done  chan collectionDone
	wake  chan struct{}

	lifecycleMu sync.Mutex
	started     bool
	running     bool
	cancel      context.CancelFunc
	stopDone    chan struct{}
	workers     sync.WaitGroup
}

type groupEntry struct {
	group       Group
	nextRunAt   time.Time
	nextCadence time.Time
	retrying    bool
	failures    uint32
	heapIndex   int
	lastPlanned time.Time
}

type RuntimeState struct {
	Started       bool
	Running       bool
	Groups        int
	InFlight      int
	Queued        int
	QueueCapacity int
}

type GroupRuntimeState struct {
	GroupID    string
	NextRunAt  time.Time
	InFlight   bool
	Failures   uint32
	Period     time.Duration
	LastPlanAt time.Time
}

func NewScheduler(source MetadataSource, executor GroupExecutor, config Config, options ...Option) (*Scheduler, error) {
	if executor == nil || reflect.ValueOf(executor).Kind() == reflect.Ptr && reflect.ValueOf(executor).IsNil() {
		return nil, errors.New("group executor is required")
	}
	if config.BatchSize == 0 {
		config.BatchSize = DefaultBatchSize
	}
	if config.WorkerCount == 0 {
		config.WorkerCount = DefaultWorkerCount
	}
	if config.QueueCapacity == 0 {
		config.QueueCapacity = DefaultQueueCapacity
	}
	if config.RetryBase == 0 {
		config.RetryBase = DefaultRetryBase
	}
	if config.RetryMax == 0 {
		config.RetryMax = DefaultRetryMax
	}
	if config.BootstrapSpread == 0 {
		config.BootstrapSpread = DefaultBootstrapSpread
	}
	if config.WorkerCount < 1 {
		return nil, fmt.Errorf("scheduler worker count must be positive: %d", config.WorkerCount)
	}
	if config.QueueCapacity < 1 {
		return nil, fmt.Errorf("scheduler queue capacity must be positive: %d", config.QueueCapacity)
	}
	if config.RetryBase < 0 || config.RetryMax < config.RetryBase {
		return nil, fmt.Errorf("invalid retry range: base=%s max=%s", config.RetryBase, config.RetryMax)
	}
	if config.BootstrapSpread < 0 {
		return nil, fmt.Errorf("bootstrap spread must not be negative: %s", config.BootstrapSpread)
	}
	builder, err := NewGroupBuilder(config.BatchSize)
	if err != nil {
		return nil, err
	}
	scheduler := &Scheduler{
		config:      config,
		source:      source,
		executor:    executor,
		builder:     builder,
		clock:       realClock{},
		entries:     make(map[string]*groupEntry),
		inFlightIDs: make(map[string]int),
		tasks:       make(chan collectionTask, config.QueueCapacity),
		done:        make(chan collectionDone, config.WorkerCount),
		wake:        make(chan struct{}, 1),
	}
	for _, option := range options {
		option(scheduler)
	}
	return scheduler, nil
}

// Start launches one heap dispatcher and a fixed worker pool. It is
// intentionally non-blocking; Stop waits for all context-aware workers.
func (s *Scheduler) Start(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	s.lifecycleMu.Lock()
	if s.started {
		s.lifecycleMu.Unlock()
		return
	}
	s.started = true
	s.running = true
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.stopDone = make(chan struct{})
	stopDone := s.stopDone
	s.lifecycleMu.Unlock()

	if s.source != nil {
		s.Reconcile(s.source.Snapshot())
	}
	for i := 0; i < s.config.WorkerCount; i++ {
		s.workers.Add(1)
		go s.runWorker(ctx)
	}
	s.workers.Add(1)
	go s.run(ctx)
	go func() {
		s.workers.Wait()
		close(stopDone)
	}()
}

func (s *Scheduler) Stop() {
	_ = s.StopContext(context.Background())
}

// StopContext cancels dispatch and in-flight context-aware collection, then
// waits only until the caller's deadline. The lifecycle waiter is created once
// at Start, so a timeout does not create an additional stranded goroutine.
func (s *Scheduler) StopContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	cancel := s.cancel
	done := s.stopDone
	s.lifecycleMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		for {
			select {
			case <-s.tasks:
			default:
				goto drained
			}
		}
	drained:
		s.mu.Lock()
		s.inFlightIDs = make(map[string]int)
		s.mu.Unlock()
		metrics.SchedulerQueueEntries.Set(0)
		metrics.SchedulerInFlight.Set(0)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ApplyMetadataDiff consumes the already-published snapshot. The diff is a
// notification; rebuilding all immutable definitions keeps reconciliation
// deterministic and does not perform another Zabbix API call.
func (s *Scheduler) ApplyMetadataDiff(_ metadata.MetadataDiff) {
	if s.source != nil {
		s.Reconcile(s.source.Snapshot())
	}
}

func (s *Scheduler) Reconcile(snapshot *metadata.MetadataSnapshot) {
	s.ReplaceGroups(s.builder.Build(snapshot))
}

func (s *Scheduler) ReplaceGroups(groups []Group) {
	now := s.clock.Now()
	desired := make(map[string]Group, len(groups))
	for _, group := range groups {
		if group.ID == "" || group.Delay <= 0 || len(group.Batches) == 0 {
			continue
		}
		desired[group.ID] = cloneGroup(group)
	}

	s.mu.Lock()
	entries := make(map[string]*groupEntry, len(desired))
	queue := make(groupHeap, 0, len(desired))
	for groupID, group := range desired {
		entry := s.entries[groupID]
		if entry == nil {
			period := CollectionPeriod(group.Delay)
			firstRun := bootstrapRunAt(now, group.ID, period, s.config.BootstrapSpread)
			entry = &groupEntry{group: group, nextRunAt: firstRun, nextCadence: firstRun.Add(period)}
		} else {
			periodChanged := CollectionPeriod(entry.group.Delay) != CollectionPeriod(group.Delay)
			definitionChanged := !groupsEqual(entry.group, group)
			entry.group = group
			if periodChanged || definitionChanged && entry.nextRunAt.IsZero() {
				period := CollectionPeriod(group.Delay)
				entry.nextRunAt = nextRunAfter(now, group.ID, period)
				entry.nextCadence = entry.nextRunAt.Add(period)
				entry.retrying = false
			}
		}
		entry.heapIndex = len(queue)
		entries[groupID] = entry
		queue = append(queue, entry)
	}
	s.entries = entries
	s.queue = queue
	heap.Init(&s.queue)
	s.mu.Unlock()
	metrics.CollectionGroups.Set(float64(len(entries)))

	if retainer, ok := s.executor.(batchRetainer); ok {
		retained := make([]Group, 0, len(desired))
		for _, group := range desired {
			retained = append(retained, group)
		}
		retainer.RetainGroups(retained)
	}
	s.notify()
}

func (s *Scheduler) State() RuntimeState {
	s.lifecycleMu.Lock()
	started := s.started
	running := s.running
	s.lifecycleMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return RuntimeState{
		Started:       started,
		Running:       running,
		Groups:        len(s.entries),
		InFlight:      totalInFlight(s.inFlightIDs),
		Queued:        len(s.tasks),
		QueueCapacity: cap(s.tasks),
	}
}

func (s *Scheduler) GroupState(groupID string) (GroupRuntimeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[groupID]
	if !ok {
		return GroupRuntimeState{}, false
	}
	return GroupRuntimeState{
		GroupID:    groupID,
		NextRunAt:  entry.nextRunAt,
		InFlight:   s.inFlightIDs[groupID] > 0,
		Failures:   entry.failures,
		Period:     CollectionPeriod(entry.group.Delay),
		LastPlanAt: entry.lastPlanned,
	}, true
}

func (s *Scheduler) run(ctx context.Context) {
	defer func() {
		s.lifecycleMu.Lock()
		s.running = false
		s.lifecycleMu.Unlock()
		s.workers.Done()
	}()
	timer := s.clock.NewTimer(s.nextWait())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			s.dispatchDue(ctx)
		case result := <-s.done:
			s.handleDone(result)
		case <-s.wake:
		}
		resetTimer(timer, s.nextWait())
	}
}

func (s *Scheduler) dispatchDue(ctx context.Context) {
	now := s.clock.Now()
	for {
		s.mu.Lock()
		if len(s.queue) == 0 || s.queue[0].nextRunAt.After(now) {
			s.mu.Unlock()
			return
		}
		entry := heap.Pop(&s.queue).(*groupEntry)
		planned := entry.nextRunAt
		lag := now.Sub(planned)
		if lag < 0 {
			lag = 0
		}
		metrics.SchedulerLag.Observe(lag.Seconds())
		entry.lastPlanned = planned
		period := CollectionPeriod(entry.group.Delay)
		if entry.retrying {
			entry.nextRunAt = entry.nextCadence
			if !entry.nextRunAt.After(now) {
				entry.nextRunAt = advancePlanned(entry.nextRunAt, period, now)
			}
			entry.retrying = false
		} else {
			entry.nextRunAt = advancePlanned(planned, period, now)
		}
		entry.nextCadence = entry.nextRunAt
		heap.Push(&s.queue, entry)

		if s.inFlightIDs[entry.group.ID] == 0 {
			task := collectionTask{
				group:       cloneGroup(entry.group),
				scheduledAt: planned.UnixNano(),
			}
			select {
			case s.tasks <- task:
				s.inFlightIDs[entry.group.ID]++
				metrics.SchedulerQueueEntries.Set(float64(len(s.tasks)))
				metrics.SchedulerInFlight.Set(float64(totalInFlight(s.inFlightIDs)))
			case <-ctx.Done():
				s.mu.Unlock()
				return
			default:
				metrics.GroupRunsTotal.WithLabelValues("queue_full", delayBucket(entry.group.Delay)).Inc()
				entry.nextCadence = entry.nextRunAt
				entry.nextRunAt = now.Add(s.config.RetryBase)
				entry.retrying = true
				heap.Fix(&s.queue, entry.heapIndex)
			}
		} else {
			metrics.GroupRunsTotal.WithLabelValues("inflight", delayBucket(entry.group.Delay)).Inc()
		}
		s.mu.Unlock()
	}
}

func (s *Scheduler) handleDone(result collectionDone) {
	now := s.clock.Now()
	s.mu.Lock()
	if s.inFlightIDs[result.groupID] > 1 {
		s.inFlightIDs[result.groupID]--
	} else {
		delete(s.inFlightIDs, result.groupID)
	}
	entry := s.entries[result.groupID]
	delay := result.delay
	if entry != nil {
		delay = entry.group.Delay
		if result.err == nil {
			entry.failures = 0
		} else {
			entry.failures++
			retryAt := now.Add(retryDelay(s.config.RetryBase, s.config.RetryMax, entry.failures))
			if retryAt.Before(entry.nextRunAt) {
				entry.nextCadence = entry.nextRunAt
				entry.nextRunAt = retryAt
				entry.retrying = true
				heap.Fix(&s.queue, entry.heapIndex)
			}
		}
	}
	metrics.SchedulerInFlight.Set(float64(totalInFlight(s.inFlightIDs)))
	s.mu.Unlock()
	status := "success"
	if result.err != nil {
		status = "error"
	}
	metrics.GroupRunsTotal.WithLabelValues(status, delayBucket(delay)).Inc()
}

func (s *Scheduler) nextWait() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return 24 * time.Hour
	}
	wait := s.queue[0].nextRunAt.Sub(s.clock.Now())
	if wait < 0 {
		return 0
	}
	return wait
}

func (s *Scheduler) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func nextRunAfter(now time.Time, groupID string, period time.Duration) time.Time {
	if period <= 0 {
		return now
	}
	periodNanos := int64(period)
	nowNanos := now.UnixNano()
	remainder := nowNanos % periodNanos
	if remainder < 0 {
		remainder += periodNanos
	}
	base := nowNanos - remainder
	candidate := unixNanoTime(base + int64(StableOffset(groupID, period)))
	if !candidate.After(now) {
		candidate = candidate.Add(period)
	}
	return candidate
}

func bootstrapRunAt(now time.Time, groupID string, period, spread time.Duration) time.Time {
	if spread <= 0 || spread > period {
		spread = period
	}
	return now.Add(StableOffset(groupID, spread))
}

func advancePlanned(planned time.Time, period time.Duration, now time.Time) time.Time {
	next := planned.Add(period)
	if next.After(now) {
		return next
	}
	missed := now.Sub(next)/period + 1
	return next.Add(missed * period)
}

func delayBucket(delay time.Duration) string {
	switch {
	case delay <= time.Minute:
		return "le_1m"
	case delay <= 5*time.Minute:
		return "le_5m"
	case delay <= time.Hour:
		return "le_1h"
	default:
		return "gt_1h"
	}
}

func retryDelay(base, maximum time.Duration, failures uint32) time.Duration {
	if base <= 0 || failures == 0 {
		return 0
	}
	delay := base
	for i := uint32(1); i < failures && delay < maximum; i++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func unixNanoTime(value int64) time.Time { return time.Unix(0, value) }

func groupsEqual(a, b Group) bool {
	return a.ID == b.ID && a.HostID == b.HostID && a.ValueType == b.ValueType &&
		a.Delay == b.Delay && a.Version == b.Version && reflect.DeepEqual(a.Batches, b.Batches)
}

func totalInFlight(values map[string]int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

type groupHeap []*groupEntry

func (h groupHeap) Len() int { return len(h) }
func (h groupHeap) Less(i, j int) bool {
	if h[i].nextRunAt.Equal(h[j].nextRunAt) {
		return h[i].group.ID < h[j].group.ID
	}
	return h[i].nextRunAt.Before(h[j].nextRunAt)
}
func (h groupHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIndex = i
	h[j].heapIndex = j
}
func (h *groupHeap) Push(value any) {
	entry := value.(*groupEntry)
	entry.heapIndex = len(*h)
	*h = append(*h, entry)
}
func (h *groupHeap) Pop() any {
	old := *h
	last := len(old) - 1
	entry := old[last]
	old[last] = nil
	entry.heapIndex = -1
	*h = old[:last]
	return entry
}
