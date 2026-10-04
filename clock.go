package subscription

import "time"

// Clock abstracts time so tests can advance it deterministically.
type Clock interface {
	Now() time.Time
}

// RealClock uses wall-clock time.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// ManualClock is advanced explicitly in tests.
type ManualClock struct {
	now time.Time
}

func NewManualClock(t time.Time) *ManualClock { return &ManualClock{now: t} }

func (c *ManualClock) Now() time.Time { return c.now }

func (c *ManualClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func (c *ManualClock) Set(t time.Time) { c.now = t }
