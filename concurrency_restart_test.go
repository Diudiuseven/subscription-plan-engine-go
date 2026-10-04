package subscription

import (
	"sync"
	"testing"
	"time"
)

// Concurrent duplicate submissions and out-of-order requests must leave the
// engine in a self-consistent state: every snapshot shows plan, status and
// billing aligned, with no half-applied change.
func TestConcurrentRequestsAreSerialized(t *testing.T) {
	eng, clock, _, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")
	clock.Set(t0.AddDate(0, 0, 15))

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n*3)
	for i := 0; i < n; i++ {
		wg.Add(3)
		go func() { // duplicate upgrade submissions (same request ID)
			defer wg.Done()
			_, err := eng.ChangePlan(ChangePlanRequest{RequestID: "up-1", AccountID: "a1", TargetPlan: "pro"})
			errs <- err
		}()
		go func() { // duplicate cancel submissions
			defer wg.Done()
			_, err := eng.Cancel(CancelRequest{RequestID: "cx-1", AccountID: "a1"})
			errs <- err
		}()
		go func() { // concurrent readers must always see a consistent snapshot
			defer wg.Done()
			s, err := eng.GetSubscription("a1")
			if err != nil {
				errs <- err
				return
			}
			if s.PlanID != "basic" && s.PlanID != "pro" {
				t.Errorf("snapshot shows unknown plan %q", s.PlanID)
			}
			if s.Status != StatusActive && s.Status != StatusPastDue {
				t.Errorf("snapshot shows half-applied status %q", s.Status)
			}
			errs <- nil
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	s, _ := eng.GetSubscription("a1")
	if s.PlanID != "pro" || !s.CancelAtPeriodEnd {
		t.Fatalf("final state wrong: %+v", s)
	}
	// The upgrade was applied exactly once despite 20 duplicate submissions.
	var proration int
	for _, inv := range eng.ListInvoices("a1") {
		if inv.Lines[0].Kind == LineProration {
			proration++
		}
	}
	if proration != 1 {
		t.Fatalf("want exactly 1 proration invoice, got %d", proration)
	}
}

// A restart must reload persisted state and continue pending changes with
// the semantics they were issued with (original EffectiveAt).
func TestRestartContinuesPendingChange(t *testing.T) {
	eng, clock, _, store := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")
	eff := t0.AddDate(0, 0, 15)
	if _, err := eng.ChangePlan(ChangePlanRequest{RequestID: "chg-1", AccountID: "a1", TargetPlan: "pro", EffectiveAt: eff}); err != nil {
		t.Fatal(err)
	}

	// Simulate restart: new engine over the same store, clock already past
	// the effective time.
	clock.Set(eff.Add(2 * time.Hour))
	eng2, err := NewEngine(clock, eng.catalog, defaultConfig(), &fakeGateway{}, store)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := eng2.GetSubscription("a1")
	if s.PendingChange == nil || !s.PendingChange.EffectiveAt.Equal(eff) {
		t.Fatalf("pending change lost across restart: %+v", s)
	}
	if err := eng2.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng2.GetSubscription("a1")
	if s.PlanID != "pro" || s.PendingChange != nil {
		t.Fatalf("pending change not applied after restart: %+v", s)
	}
	// Proration is computed from the original EffectiveAt (half period),
	// not from the restart time.
	invs := eng2.ListInvoices("a1")
	last := invs[len(invs)-1]
	if last.AmountCents != 3000 {
		t.Fatalf("proration must use issued semantics (EffectiveAt): %+v", last)
	}
}

// Out-of-order arrival: a cancel issued after a scheduled change, then the
// change's effective time passes — the deterministic outcome is that the
// change applies (mid-period) and the subscription still ends at period end.
func TestCancelAndScheduledChangeOrdering(t *testing.T) {
	eng, clock, _, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")
	eff := t0.AddDate(0, 0, 15)
	if _, err := eng.ChangePlan(ChangePlanRequest{RequestID: "chg-1", AccountID: "a1", TargetPlan: "pro", EffectiveAt: eff}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Cancel(CancelRequest{RequestID: "cx-1", AccountID: "a1"}); err != nil {
		t.Fatal(err)
	}
	clock.Set(eff)
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ := eng.GetSubscription("a1")
	if s.PlanID != "pro" || !s.CancelAtPeriodEnd {
		t.Fatalf("change should apply, cancel still pending: %+v", s)
	}
	clock.Set(t0.AddDate(0, 0, 30))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng.GetSubscription("a1")
	if s.Status != StatusEnded {
		t.Fatalf("want ended at period end, got %s", s.Status)
	}
}
