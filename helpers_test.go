package subscription

import (
	"errors"
	"sync"
	"testing"
	"time"
)

var errCharge = errors.New("charge declined")

// fakeGateway simulates a payment gateway with switchable failures.
type fakeGateway struct {
	mu      sync.Mutex
	fail    bool
	charges []int64
}

func (g *fakeGateway) Charge(accountID string, amountCents int64, invoiceID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail {
		return errCharge
	}
	g.charges = append(g.charges, amountCents)
	return nil
}

func (g *fakeGateway) setFail(f bool) { g.mu.Lock(); g.fail = f; g.mu.Unlock() }

func (g *fakeGateway) total() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var sum int64
	for _, c := range g.charges {
		sum += c
	}
	return sum
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newTestEngine builds an engine with a two-plan catalog (basic rank 1,
// pro rank 2), 14-day trials, 30-day cycles, and a manual clock at t0.
func newTestEngine(t *testing.T, cfg Config) (*Engine, *ManualClock, *fakeGateway, *MemStore) {
	t.Helper()
	clock := NewManualClock(t0)
	cat := NewCatalog()
	cat.UpsertPlan(Plan{ID: "basic", Name: "Basic", Rank: 1, TrialDays: 14})
	cat.UpsertPlan(Plan{ID: "pro", Name: "Pro", Rank: 2, TrialDays: 14})
	if err := cat.SetPrice("basic", PriceVersion{ID: "basic-v1", AmountCents: 3000, IntervalDays: 30, EffectiveFrom: t0.AddDate(0, 0, -365)}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SetPrice("pro", PriceVersion{ID: "pro-v1", AmountCents: 9000, IntervalDays: 30, EffectiveFrom: t0.AddDate(0, 0, -365)}); err != nil {
		t.Fatal(err)
	}
	gw := &fakeGateway{}
	store := NewMemStore()
	eng, err := NewEngine(clock, cat, cfg, gw, store)
	if err != nil {
		t.Fatal(err)
	}
	return eng, clock, gw, store
}

func defaultConfig() Config {
	return Config{
		GracePeriod:      72 * time.Hour,
		Retry:            RetryPolicy{Intervals: []time.Duration{24 * time.Hour, 24 * time.Hour}},
		OnRetryExhausted: ActionSuspend,
	}
}

// startAndConvert is a shortcut: start a trial and convert it immediately.
func startAndConvert(t *testing.T, e *Engine, account, plan string) Subscription {
	t.Helper()
	if _, err := e.StartTrial(StartTrialRequest{RequestID: account + "-trial", AccountID: account, PlanID: plan}); err != nil {
		t.Fatal(err)
	}
	s, err := e.ConvertTrial(ConvertTrialRequest{RequestID: account + "-convert", AccountID: account})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
