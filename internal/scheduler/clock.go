package scheduler

import "time"

// Clock and Timer keep scheduling tests independent of wall-clock sleeps.
type Clock interface {
	Now() time.Time
	NewTimer(delay time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(delay time.Duration) bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(delay time.Duration) Timer {
	return &realTimer{timer: time.NewTimer(delay)}
}

type realTimer struct {
	timer *time.Timer
}

func (t *realTimer) C() <-chan time.Time            { return t.timer.C }
func (t *realTimer) Stop() bool                     { return t.timer.Stop() }
func (t *realTimer) Reset(delay time.Duration) bool { return t.timer.Reset(delay) }

func resetTimer(timer Timer, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	if !timer.Stop() {
		select {
		case <-timer.C():
		default:
		}
	}
	timer.Reset(delay)
}
