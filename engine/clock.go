package engine

import "time"

// Clock 时间源，便于测试中注入确定性时间。
type Clock interface {
	Now() time.Time
}

// RealClock 真实时钟。
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// ManualClock 手动推进的时钟，用于测试。
type ManualClock struct {
	now time.Time
}

func NewManualClock(t time.Time) *ManualClock { return &ManualClock{now: t} }

func (c *ManualClock) Now() time.Time { return c.now }

func (c *ManualClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func (c *ManualClock) Set(t time.Time) { c.now = t }
