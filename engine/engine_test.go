package engine

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func testPlans() []Plan {
	return []Plan{
		{ID: "trial-basic", Name: "Basic w/ Trial", PriceCents: 1000, IntervalDays: 30, TrialDays: 14},
		{ID: "basic", Name: "Basic", PriceCents: 1000, IntervalDays: 30},
		{ID: "pro", Name: "Pro", PriceCents: 3000, IntervalDays: 30},
	}
}

func defaultCfg() Config {
	return Config{GracePeriodDays: 7, MaxRetries: 3, RetryIntervalDays: 2, OnExhausted: PolicySuspend}
}

func newTestEngine(clock *ManualClock) *Engine {
	return NewEngine(NewMemoryStore(), clock, defaultCfg(), testPlans())
}

func TestTrialConversion(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)

	sub, err := e.Subscribe("acct1", "trial-basic", "")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != StatusTrialing {
		t.Fatalf("want trialing, got %s", sub.Status)
	}
	wantTrialEnd := t0.AddDate(0, 0, 14)
	if !sub.TrialEnd.Equal(wantTrialEnd) {
		t.Fatalf("trial end = %v, want %v", sub.TrialEnd, wantTrialEnd)
	}

	// 到期前推进：不转化
	if err := e.Advance(t0.AddDate(0, 0, 13)); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusTrialing {
		t.Fatalf("premature conversion: %s", sub.Status)
	}

	// 到期：转化为 active 并出账
	if err := e.Advance(wantTrialEnd); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusActive {
		t.Fatalf("want active, got %s", sub.Status)
	}
	if !sub.CurrentPeriodStart.Equal(wantTrialEnd) {
		t.Fatalf("period start = %v", sub.CurrentPeriodStart)
	}
	invs := e.Invoices(sub.ID)
	if len(invs) != 1 || invs[0].AmountCents != 1000 || !invs[0].Paid {
		t.Fatalf("invoices = %+v", invs)
	}

	// 幂等：同一时刻重复 Advance 不产生二次迁移
	v := sub.Version
	if err := e.Advance(wantTrialEnd); err != nil {
		t.Fatal(err)
	}
	sub2, _ := e.Get(sub.ID)
	if sub2.Version != v || len(e.Invoices(sub.ID)) != 1 {
		t.Fatalf("duplicate transition: version %d->%d, invoices=%d", v, sub2.Version, len(e.Invoices(sub.ID)))
	}
}

func TestUpgradeProrationDebit(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)

	sub, err := e.Subscribe("acct1", "basic", "")
	if err != nil {
		t.Fatal(err)
	}
	// 第 10 天升级到 pro：旧套餐剩余价值 1000*20/30=667，新套餐剩余成本 3000*20/30=2000，补差 1333
	clock.Set(t0.AddDate(0, 0, 10))
	sub, err = e.ChangePlan(sub.ID, "pro", time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if sub.PlanID != "pro" {
		t.Fatalf("plan = %s", sub.PlanID)
	}
	if sub.OutstandingCents != 0 {
		t.Fatalf("outstanding = %d, want 0 (charger succeeds)", sub.OutstandingCents)
	}
	var debit int64
	for _, le := range e.Ledger(sub.ID) {
		if le.Kind == "proration_debit" {
			debit = le.Amount
		}
	}
	if debit != 1333 {
		t.Fatalf("proration debit = %d, want 1333", debit)
	}
}

func TestDowngradeCreditAndReconcile(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)

	sub, err := e.Subscribe("acct1", "pro", "")
	if err != nil {
		t.Fatal(err)
	}
	// 第 15 天降级到 basic：旧剩余 3000*15/30=1500，新成本 1000*15/30=500，结余 1000
	clock.Set(t0.AddDate(0, 0, 15))
	sub, err = e.ChangePlan(sub.ID, "basic", time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if sub.CreditCents != 1000 {
		t.Fatalf("credit = %d, want 1000", sub.CreditCents)
	}

	// 周期结束续费：1000 应收全部用结余抵扣
	if err := e.Advance(t0.AddDate(0, 0, 30)); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusActive || sub.CreditCents != 0 {
		t.Fatalf("status=%s credit=%d", sub.Status, sub.CreditCents)
	}
	invs := e.Invoices(sub.ID)
	if len(invs) != 2 {
		t.Fatalf("invoices = %d, want 2", len(invs))
	}
	if invs[1].AmountCents != 0 || !invs[1].Paid {
		t.Fatalf("renewal invoice = %+v, want 0 paid", invs[1])
	}

	// 对账：实收总额 == 逐段重算口径（pro 15天 + basic 15天 + basic 下一整周期）
	var charged int64
	for _, le := range e.Ledger(sub.ID) {
		if le.Kind == "charge" {
			charged += le.Amount
		}
	}
	want := int64(1500 + 500 + 1000)
	if charged != want {
		t.Fatalf("charged = %d, want %d (reconciled per-period)", charged, want)
	}
}
