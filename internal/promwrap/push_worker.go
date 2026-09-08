package promwrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"zabbix-exporter/internal/cache"
)

const (
	RemoteWriteVersion      = "0.1.0"
	ContentType             = "application/x-protobuf"
	maxResponseSummaryBytes = 4096
)

// DeliveryClass determines whether a Remote Write failure may be retried.
type DeliveryClass uint8

const (
	DeliverySuccess DeliveryClass = iota
	DeliveryRetryable
	DeliveryPermanent
)

func (c DeliveryClass) String() string {
	switch c {
	case DeliverySuccess:
		return "success"
	case DeliveryRetryable:
		return "retryable"
	case DeliveryPermanent:
		return "permanent"
	default:
		return "unknown"
	}
}

// ClassifyRemoteWriteStatus applies the single-endpoint HTTP retry policy.
func ClassifyRemoteWriteStatus(statusCode int) DeliveryClass {
	if statusCode >= 200 && statusCode < 300 {
		return DeliverySuccess
	}
	if statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests || statusCode >= 500 {
		return DeliveryRetryable
	}
	return DeliveryPermanent
}

// DeliveryError is returned after a permanent failure or after retry budget is
// exhausted. ResponseSummary is deliberately bounded.
type DeliveryError struct {
	Class           DeliveryClass
	StatusCode      int
	ResponseSummary string
	Attempts        int
	Cause           error
}

func (e *DeliveryError) Error() string {
	if e.StatusCode != 0 {
		if e.ResponseSummary != "" {
			return fmt.Sprintf("remote write HTTP %d (%s) after %d attempt(s): %s", e.StatusCode, e.Class, e.Attempts, e.ResponseSummary)
		}
		return fmt.Sprintf("remote write HTTP %d (%s) after %d attempt(s)", e.StatusCode, e.Class, e.Attempts)
	}
	return fmt.Sprintf("remote write transport failure (%s) after %d attempt(s): %v", e.Class, e.Attempts, e.Cause)
}

func (e *DeliveryError) Unwrap() error { return e.Cause }

type singleEndpoint struct {
	url      string
	timeout  time.Duration
	username string
	password string
}

func (e singleEndpoint) validate() error {
	parsed, err := url.ParseRequestURI(e.url)
	if err != nil {
		return fmt.Errorf("invalid remote write URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("remote write URL scheme must be http or https: %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("remote write URL must include a host")
	}
	if e.timeout <= 0 {
		return fmt.Errorf("remote write timeout must be positive: %s", e.timeout)
	}
	return nil
}

type pushWorker struct {
	endpoint   singleEndpoint
	client     *http.Client
	maxRetries int
	backoff    time.Duration
	maxBackoff time.Duration
}

type deliveryResult struct {
	attempts int
	class    DeliveryClass
	err      error
}

func (w *pushWorker) send(ctx context.Context, batch *encodedBatch) deliveryResult {
	var last *DeliveryError
	for attempt := 1; attempt <= w.maxRetries+1; attempt++ {
		last = w.sendAttempt(ctx, batch, attempt)
		if last == nil {
			return deliveryResult{attempts: attempt, class: DeliverySuccess}
		}
		if last.Class != DeliveryRetryable || attempt > w.maxRetries {
			last.Attempts = attempt
			return deliveryResult{attempts: attempt, class: last.Class, err: last}
		}
		if err := waitRetry(ctx, boundedBackoff(w.backoff, w.maxBackoff, attempt-1)); err != nil {
			last.Attempts = attempt
			last.Cause = errors.Join(last.Cause, err)
			return deliveryResult{attempts: attempt, class: DeliveryRetryable, err: last}
		}
	}
	panic("unreachable")
}

func (w *pushWorker) sendAttempt(ctx context.Context, batch *encodedBatch, attempt int) *DeliveryError {
	attemptCtx, cancel := context.WithTimeout(ctx, w.endpoint.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, w.endpoint.url, bytes.NewReader(batch.body))
	if err != nil {
		return &DeliveryError{Class: DeliveryPermanent, Attempts: attempt, Cause: err}
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", RemoteWriteVersion)
	if w.endpoint.username != "" {
		req.SetBasicAuth(w.endpoint.username, w.endpoint.password)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return &DeliveryError{Class: DeliveryRetryable, Attempts: attempt, Cause: err}
	}
	defer resp.Body.Close()

	summaryBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSummaryBytes+1))
	if len(summaryBytes) > maxResponseSummaryBytes {
		summaryBytes = summaryBytes[:maxResponseSummaryBytes]
	}
	summary := strings.TrimSpace(string(summaryBytes))
	class := ClassifyRemoteWriteStatus(resp.StatusCode)
	if class == DeliverySuccess && readErr == nil {
		return nil
	}
	if readErr != nil && class == DeliverySuccess {
		class = DeliveryRetryable
	}
	return &DeliveryError{
		Class:           class,
		StatusCode:      resp.StatusCode,
		ResponseSummary: summary,
		Attempts:        attempt,
		Cause:           readErr,
	}
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func boundedBackoff(base, maximum time.Duration, exponent int) time.Duration {
	if base <= 0 {
		return 0
	}
	delay := base
	for i := 0; i < exponent; i++ {
		if delay >= maximum || delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if maximum > 0 && delay > maximum {
		return maximum
	}
	return delay
}

type queueAdmission uint8

const (
	queueAccepted queueAdmission = iota
	queueFull
	queueObsolete
)

// laneQueue is one globally bounded logical queue split into stable worker
// lanes. Each lane has exactly one consumer, preserving per-series FIFO order.
type laneQueue struct {
	mu           sync.Mutex
	lanes        [][]*encodedBatch
	notify       []chan struct{}
	capacity     int
	size         int
	closed       bool
	latestCycles map[uint16]int64
}

func newLaneQueue(workers, capacity int) *laneQueue {
	notify := make([]chan struct{}, workers)
	for i := range notify {
		notify[i] = make(chan struct{}, 1)
	}
	return &laneQueue{
		lanes:        make([][]*encodedBatch, workers),
		notify:       notify,
		capacity:     capacity,
		latestCycles: make(map[uint16]int64),
	}
}

func (q *laneQueue) beginLongCycle(slot uint16, cycleMS int64) (bool, []*encodedBatch) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false, nil
	}
	latest, exists := q.latestCycles[slot]
	if exists && cycleMS < latest {
		return false, nil
	}
	if exists && cycleMS == latest {
		return true, nil
	}
	q.latestCycles[slot] = cycleMS

	var removed []*encodedBatch
	for laneIndex := range q.lanes {
		lane := q.lanes[laneIndex]
		kept := lane[:0]
		for _, batch := range lane {
			if batch.policy == cache.PublishCycleTimestamp && batch.slot == slot && batch.cycleTimestampMS < cycleMS {
				removed = append(removed, batch)
				q.size--
				continue
			}
			kept = append(kept, batch)
		}
		for i := len(kept); i < len(lane); i++ {
			lane[i] = nil
		}
		if len(kept) == 0 {
			q.lanes[laneIndex] = nil
		} else {
			q.lanes[laneIndex] = kept
		}
	}
	return true, removed
}

func (q *laneQueue) tryEnqueue(batch *encodedBatch) queueAdmission {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return queueObsolete
	}
	if batch.policy == cache.PublishCycleTimestamp && batch.cycleTimestampMS < q.latestCycles[batch.slot] {
		return queueObsolete
	}
	if q.size >= q.capacity {
		return queueFull
	}
	q.lanes[batch.lane] = append(q.lanes[batch.lane], batch)
	q.size++
	select {
	case q.notify[batch.lane] <- struct{}{}:
	default:
	}
	return queueAccepted
}

func (q *laneQueue) pop(ctx context.Context, lane int) (*encodedBatch, bool) {
	for {
		q.mu.Lock()
		if len(q.lanes[lane]) != 0 {
			batch := q.lanes[lane][0]
			q.lanes[lane][0] = nil
			q.lanes[lane] = q.lanes[lane][1:]
			if len(q.lanes[lane]) == 0 {
				q.lanes[lane] = nil
			}
			q.size--
			if len(q.lanes[lane]) != 0 {
				select {
				case q.notify[lane] <- struct{}{}:
				default:
				}
			}
			q.mu.Unlock()
			return batch, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-q.notify[lane]:
		}
	}
}

func (q *laneQueue) close() []*encodedBatch {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	var remaining []*encodedBatch
	for i := range q.lanes {
		remaining = append(remaining, q.lanes[i]...)
		q.lanes[i] = nil
		select {
		case q.notify[i] <- struct{}{}:
		default:
		}
	}
	q.size = 0
	return remaining
}

func (q *laneQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.size
}
