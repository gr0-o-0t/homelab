package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackoff_ExponentialThenGiveUp(t *testing.T) {
	b := NewBackoff()
	now := time.Unix(1_000_000, 0)
	var delays []time.Duration
	for i := range 5 {
		d, giveUp := b.Next("x", now.Add(time.Duration(i)*time.Second))
		assert.False(t, giveUp, "attempt %d", i+1)
		delays = append(delays, d)
	}
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}, delays)
	_, giveUp := b.Next("x", now.Add(10*time.Second))
	assert.True(t, giveUp, "sixth failure within ten minutes")

	// Other containers are counted separately.
	d, giveUp := b.Next("y", now)
	assert.False(t, giveUp)
	assert.Equal(t, time.Second, d)

	// Once the window has passed, it starts over.
	d, giveUp = b.Next("x", now.Add(30*time.Minute))
	assert.False(t, giveUp)
	assert.Equal(t, time.Second, d)
}

func TestBackoff_CappedAtMax(t *testing.T) {
	b := &Backoff{Base: time.Second, Max: time.Minute, Window: time.Hour, Limit: 20}
	now := time.Unix(0, 0)
	var d time.Duration
	for range 10 {
		d, _ = b.Next("x", now)
	}
	assert.Equal(t, time.Minute, d)
}
