package scheduler

import (
	"context"
	"time"

	"github.com/zhaeng/zabbix-exporter/internal/metrics"
)

type collectionTask struct {
	group       Group
	scheduledAt int64
}

type collectionDone struct {
	groupID string
	delay   time.Duration
	err     error
}

func (s *Scheduler) runWorker(ctx context.Context) {
	defer s.workers.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-s.tasks:
			metrics.SchedulerQueueEntries.Set(float64(len(s.tasks)))
			// Anchor the history window to actual execution time. A task may wait
			// in the bounded scheduler queue long enough for short-cycle values
			// selected at dispatch time to have already expired.
			queryTill := s.clock.Now()
			err := s.executor.Execute(ctx, task.group, queryTill)
			select {
			case s.done <- collectionDone{groupID: task.group.ID, delay: task.group.Delay, err: err}:
			case <-ctx.Done():
				return
			}
		}
	}
}
