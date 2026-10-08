package session

import "time"

// Backoff paces restarts of a crashing container: exponential delays from
// Base up to Max, and after Limit restarts within Window it gives up, so a
// container that cannot stay up does not restart forever. Not safe for
// concurrent use; the supervisor holds its lock around it.
type Backoff struct {
	Base, Max, Window time.Duration
	Limit             int
	hist              map[string][]time.Time
}

// NewBackoff is the supervisor's policy: 1s → 60s, five restarts per ten
// minutes.
func NewBackoff() *Backoff {
	return &Backoff{Base: time.Second, Max: time.Minute, Window: 10 * time.Minute, Limit: 5}
}

// Next records a failure of key at now and returns how long to wait before
// the next restart, or giveUp once more than Limit failures fell within
// Window.
func (b *Backoff) Next(key string, now time.Time) (delay time.Duration, giveUp bool) {
	if b.hist == nil {
		b.hist = map[string][]time.Time{}
	}
	var kept []time.Time
	for _, t := range b.hist[key] {
		if now.Sub(t) < b.Window {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	b.hist[key] = kept
	n := len(kept)
	if n > b.Limit {
		return 0, true
	}
	delay = b.Base
	for i := 1; i < n && delay < b.Max; i++ {
		delay *= 2
	}
	if delay > b.Max {
		delay = b.Max
	}
	return delay, false
}
