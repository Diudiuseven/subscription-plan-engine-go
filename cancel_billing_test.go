package subscription

import (
	"testing"
	"time"
)

func TestCancelEndsAtPeriodEnd(t *testing.T) {
	eng, clock, _, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")

	s, err := eng.Cancel(CancelRequest{RequestID: "x1", AccountID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.CancelAtPeriodEnd || s.Status != StatusActive {
		t.Fatalf("cancel must keep active until period end: %+v", s)
	}
	// Duplicate cancel is a no-op.
	if _, err := eng.Cancel(CancelRequest{RequestID: "x1", AccountID: "a1"}); err != nil {
		t.Fatal(err)
	}
	// Entitlement retained duringuring thenotice period.
	if ent, _ := eng.Entitlement("a1"); !ent.Entitled {
		t.Fatal("entitlement must continue until period end")
	}
	// At period end the subscription terminates instead of renewing.
	clock.Set(t0.AddDate(0, 0, 30))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng.GetSubscription("a1")
	if s.Status != StatusEnded {
		t.Fatalf("want ended, got %s", s.Status)
	}
	if invs := eng.ListInvoices("a1"); len(invs) != 1 {
		t.Fatalf("no renewal invoice may be issued after cancel: %d", len(invs))
	}
}

func TestResumeBeforePeriodEnd(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")
	if _, err := eng.Cancel(CancelRequest{RequestID: "x1", AccountID: "a1"}); err != nil {
		t.Fatal(err)
	}
	s, err := eng.Resume(ResumeRequest{RequestID: "rs1", AccountID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.CancelAtPeriodEnd {
		t.Fatal("resume must clear the cancel flag")
	}
	clock.Set(t0.AddDate(0, 0, 30))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng.GetSubscription("a1")
	if s.Status != StatusActive || gw.total() != 6000 {
		t.Fatalf("resumed subscription must renew: %+v charges=%d", s, gw.total())
	}
}

func TestFailedChargeRetriesAndGrace(t *testing.T) {
	cfg := defaultConfig()
	cfg.Retry = RetryPolicy{Intervals: []time.Duration{24 * time.Hour}}
	cfg.GracePeriod = 72 * time.Hour
	eng, clock, gw, _ := newTestEngine(t, cfg)
	startAndConvert(t, eng, "a1", "basic")

	// Renewal fails.
	clock.Set(t0.AddDate(0, 0, 30))
	gw.setFail(true)
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ := eng.GetSubscription("a1")
	if s.Status != StatusPastDue {
		t.Fatalf("want past_due, got %s", s.Status)
	}
	// Entitlement retained during grace.
	if ent, _ := eng.Entitlement("a1"); !ent.Entitled {
		t.Fatal("grace period must retain entitlement")
	}
	// Retry at +24h fails again; retries exhausted, grace still open.
	clock.Advance(24 * time.Hour)
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	if s, _ = eng.GetSubscription("a1"); s.Status != StatusPastDue {
		t.Fatalf("still in grace, want past_due, got %s", s.Status)
	}
	// At grace end the final attempt fails -> suspended, no entitlement.
	clock.Set(t0.AddDate(0, 0, 30).Add(72 * time.Hour))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng.GetSubscription("a1")
	if s.Status != StatusSuspended {
		t.Fatalf("want suspended, got %s", s.Status)
	}
	if ent, _ := eng.Entitlement("a1"); ent.Entitled {
		t.Fatal("suspended must have no entitlement")
	}
	// Gateway recovers; the next scheduled retry settles the SAME invoice
	// (no new arrears), and the subscription comes back active.
	gw.setFail(false)
	clock.Advance(24 * time.Hour)
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng.GetSubscription("a1")
	if s.Status != StatusActive {
		t.Fatalf("want active after recovery, got %s", s.Status)
	}
	// Exactly one renewal invoice exists; recovery did not double-bill.
	var open, paid int
	for _, inv := range eng.ListInvoices("a1") {
		switch inv.Status {
		case InvoiceOpen:
			open++
		case InvoicePaid:
			paid++
		}
	}
	if open != 0 {
		t.Fatalf("no invoice may stay open after recovery: %d open", open)
	}
	if got := gw.total(); got != 3000+3000 {
		t.Fatalf("arrears double-counted: total charged %d", got)
	}
}

func TestRetryExhaustedTerminate(t *testing.T) {
	cfg := defaultConfig()
	cfg.Retry = RetryPolicy{Intervals: nil}
	cfg.GracePeriod = 0
	cfg.OnRetryExhausted = ActionTerminate
	eng, clock, gw, _ := newTestEngine(t, cfg)
	startAndConvert(t, eng, "a1", "basic")
	clock.Set(t0.AddDate(0, 0, 30))
	gw.setFail(true)
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	if err := eng.ProcessDue(); err != nil { // final attempt at grace end (== now)
		t.Fatal(err)
	}
	s, _ := eng.GetSubscription("a1")
	if s.Status != StatusEnded {
		t.Fatalf("want ended after termination policy, got %s", s.Status)
	}
}

func TestRenewalUsesPriceEffectiveAtRenewal(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")
	// Price rises mid-period; old invoices keep the old price.
	if err := eng.catalog.SetPrice("basic", PriceVersion{ID: "basic-v2", AmountCents: 3600, IntervalDays: 30, EffectiveFrom: t0.AddDate(0, 0, 15)}); err != nil {
		t.Fatal(err)
	}
	clock.Set(t0.AddDate(0, 0, 30))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	invs := eng.ListInvoices("a1")
	if invs[0].AmountCents != 3000 {
		t.Fatalf("historical invoice must not change: %+v", invs[0])
	}
	if invs[1].AmountCents != 3600 {
		t.Fatalf("renewal must use the price effective at renewal: %+v", invs[1])
	}
	if got := gw.total(); got != 3000+3600 {
		t.Fatalf("charges wrong: %d", got)
	}
}
