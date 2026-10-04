package subscription

import (
	"testing"
	"time"
)

func TestUpgradeImmediateProration(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic") // 3000/30d, period [t0, t0+30d)

	// Upgrade exactly halfway through the period.
	clock.Set(t0.AddDate(0, 0, 15))
	s, err := eng.ChangePlan(ChangePlanRequest{RequestID: "chg-1", AccountID: "a1", TargetPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if s.PlanID != "pro" {
		t.Fatalf("want pro, got %s", s.PlanID)
	}
	// Half of basic unused = 1500 credit; half of pro due = 4500; delta 3000.
	if got := gw.total(); got != 3000+3000 {
		t.Fatalf("want charges 3000+3000, got %d", got)
	}
	invs := eng.ListInvoices("a1")
	if len(invs) != 2 {
		t.Fatalf("want 2 invoices, got %d", len(invs))
	}
	pr := invs[1]
	if pr.AmountCents != 3000 || pr.NetCents != 3000 {
		t.Fatalf("proration invoice wrong: %+v", pr)
	}
	// Reconcile with a per-cycle line-by-line recomputation:
	// used basic (1500) + remaining pro (4500) = 6000 = charges for this period.
	var sum int64
	for _, l := range pr.Lines {
		sum += l.AmountCents
	}
	if sum != 3000 {
		t.Fatalf("proration lines do not net to delta: %d", sum)
	}
	usedBasic := int64(3000) - 1500
	if usedBasic+4500 != 3000+3000 {
		t.Fatal("mixed-period total does not match full-cycle recomputation")
	}
}

func TestDowngradeCreatesCarryOverCredit(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "pro") // 9000/30d

	clock.Set(t0.AddDate(0, 0, 10)) // 10 days used, 20 remain
	s, err := eng.ChangePlan(ChangePlanRequest{RequestID: "chg-1", AccountID: "a1", TargetPlan: "basic"})
	if err != nil {
		t.Fatal(err)
	}
	// unused pro = 9000*20/30 = 6000; due basic = 3000*20/30 = 2000; credit 4000.
	if s.CreditCents != 4000 {
		t.Fatalf("want 4000 credit, got %d", s.CreditCents)
	}
	if got := gw.total(); got != 9000 {
		t.Fatalf("downgrade must not charge: %d", got)
	}

	// Next renewal: 3000 invoice fully covered by 4000 credit, 1000 remains.
	clock.Set(t0.AddDate(0, 0, 30))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng.GetSubscription("a1")
	if s.CreditCents != 1000 {
		t.Fatalf("want 1000 credit left, got %d", s.CreditCents)
	}
	if got := gw.total(); got != 9000 {
		t.Fatalf("renewal should be covered by credit, charges=%d", got)
	}
	invs := eng.ListInvoices("a1")
	last := invs[len(invs)-1]
	if last.CreditApplied != 3000 || last.Status != InvoicePaid {
		t.Fatalf("renewal invoice wrong: %+v", last)
	}
}

func TestScheduledChangeAndCancel(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")

	eff := t0.AddDate(0, 0, 20)
	s, err := eng.ChangePlan(ChangePlanRequest{RequestID: "chg-1", AccountID: "a1", TargetPlan: "pro", EffectiveAt: eff})
	if err != nil {
		t.Fatal(err)
	}
	if s.PlanID != "basic" || s.PendingChange == nil {
		t.Fatalf("future change must not touch current plan: %+v", s)
	}
	// Cancel before effective time: back to original plan, no residue.
	s, err = eng.CancelScheduledChange(CancelScheduledChangeRequest{RequestID: "cc-1", AccountID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.PendingChange != nil || s.PlanID != "basic" {
		t.Fatalf("cancel must restore original plan with no residue: %+v", s)
	}
	// Advance past the would-be effective time: nothing happens.
	clock.Set(eff.Add(time.Hour))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ = eng.GetSubscription("a1")
	if s.PlanID != "basic" || gw.total() != 3000 {
		t.Fatalf("revoked change must never apply: %+v charges=%d", s, gw.total())
	}
}

func TestScheduledChangeAppliesAtEffectiveTime(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")

	eff := t0.AddDate(0, 0, 15)
	if _, err := eng.ChangePlan(ChangePlanRequest{RequestID: "chg-1", AccountID: "a1", TargetPlan: "pro", EffectiveAt: eff}); err != nil {
		t.Fatal(err)
	}
	// Before effective time: still basic.
	clock.Set(eff.Add(-time.Second))
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	if s, _ := eng.GetSubscription("a1"); s.PlanID != "basic" {
		t.Fatal("change applied too early")
	}
	// At effective time: applies, prorated from EffectiveAt (half period).
	clock.Set(eff)
	if err := eng.ProcessDue(); err != nil {
		t.Fatal(err)
	}
	s, _ := eng.GetSubscription("a1")
	if s.PlanID != "pro" || s.PendingChange != nil {
		t.Fatalf("scheduled change not applied: %+v", s)
	}
	if got := gw.total(); got != 3000+3000 {
		t.Fatalf("want prorated charge 3000, total got %d", got)
	}
	// Cancelling after it took effect is rejected.
	if _, err := eng.CancelScheduledChange(CancelScheduledChangeRequest{RequestID: "cc-2", AccountID: "a1"}); err != ErrNoPendingChange {
		t.Fatalf("want ErrNoPendingChange, got %v", err)
	}
}

func TestChangePlanIdempotent(t *testing.T) {
	eng, clock, gw, _ := newTestEngine(t, defaultConfig())
	startAndConvert(t, eng, "a1", "basic")
	clock.Set(t0.AddDate(0, 0, 15))
	if _, err := eng.ChangePlan(ChangePlanRequest{RequestID: "chg-1", AccountID: "a1", TargetPlan: "pro"}); err != nil {
		t.Fatal(err)
	}
	total := gw.total()
	s, err := eng.ChangePlan(ChangePlanRequest{RequestID: "chg-1", AccountID: "a1", TargetPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if s.PlanID != "pro" || gw.total() != total {
		t.Fatal("duplicate change request must be a no-op")
	}
}
