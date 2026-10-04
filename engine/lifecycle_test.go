package engine

import (
	"testing"
	"time"
)

func TestFutureChangeAndCancel(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)

	sub, _ := e.Subscribe("acct1", "basic", "")
	eff := t0.AddDate(0, 0, 10)

	// 指定未来时刻生效
	sub, err := e.ChangePlan(sub.ID, "pro", eff, "")
	if err != nil {
		t.Fatal(err)
	}
	if sub.PlanID != "basic" || sub.PendingChange == nil {
		t.Fatalf("plan=%s pending=%+v", sub.PlanID, sub.PendingChange)
	}
	if !sub.PendingChange.EffectiveAt.Equal(eff) || sub.PendingChange.Kind != ChangeUpgrade {
		t.Fatalf("pending = %+v", sub.PendingChange)
	}

	// 到期前撤销：回到原套餐，无中间状态残留
	sub, err = e.CancelPendingChange(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.PendingChange != nil || sub.PlanID != "basic" {
		t.Fatalf("after cancel: plan=%s pending=%+v", sub.PlanID, sub.PendingChange)
	}
	if _, err := e.CancelPendingChange(sub.ID); err != ErrNoPendingChange {
		t.Fatalf("double cancel = %v", err)
	}
	// 推进越过原生效时刻：不应发生任何变更
	if err := e.Advance(eff.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.PlanID != "basic" {
		t.Fatalf("plan changed after cancel: %s", sub.PlanID)
	}
	for _, le := range e.Ledger(sub.ID) {
		if le.Kind == "proration_debit" || le.Kind == "proration_credit" {
			t.Fatalf("residual proration entry: %+v", le)
		}
	}
}

func TestFutureChangeExecutes(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)

	sub, _ := e.Subscribe("acct1", "basic", "")
	eff := t0.AddDate(0, 0, 10)
	if _, err := e.ChangePlan(sub.ID, "pro", eff, ""); err != nil {
		t.Fatal(err)
	}
	// 生效前推进不变更
	if err := e.Advance(eff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.PlanID != "basic" {
		t.Fatalf("premature change: %s", sub.PlanID)
	}
	// 生效时刻：按 eff 的剩余时长折算（与立即变更同口径：补差 1333）
	if err := e.Advance(eff); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.PlanID != "pro" || sub.PendingChange != nil {
		t.Fatalf("plan=%s pending=%+v", sub.PlanID, sub.PendingChange)
	}
	var debit int64
	for _, le := range e.Ledger(sub.ID) {
		if le.Kind == "proration_debit" {
			debit = le.Amount
		}
	}
	if debit != 1333 {
		t.Fatalf("debit = %d, want 1333", debit)
	}
}

func TestCancelAtPeriodEndAndResume(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)

	sub, _ := e.Subscribe("acct1", "basic", "")

	// 取消：周期内权益保留
	sub, err := e.Cancel(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != StatusCanceled || !sub.CancelAtPeriodEnd {
		t.Fatalf("status=%s", sub.Status)
	}
	if !Entitled(sub, t0.AddDate(0, 0, 15)) {
		t.Fatal("should be entitled until period end")
	}

	// 恢复：撤销取消
	sub, err = e.Resume(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != StatusActive || sub.CancelAtPeriodEnd {
		t.Fatalf("after resume: %s", sub.Status)
	}

	// 再次取消并推进到周期结束：订阅终止，不再续费
	if _, err := e.Cancel(sub.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(t0.AddDate(0, 0, 30)); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusExpired {
		t.Fatalf("want expired, got %s", sub.Status)
	}
	if Entitled(sub, t0.AddDate(0, 0, 31)) {
		t.Fatal("expired sub must not be entitled")
	}
	// 推进更远：无新账单、无状态迁移
	if err := e.Advance(t0.AddDate(0, 0, 90)); err != nil {
		t.Fatal(err)
	}
	if n := len(e.Invoices(sub.ID)); n != 1 {
		t.Fatalf("invoices after expiry = %d, want 1", n)
	}
}

func TestCancelRenewRaceDeterministic(t *testing.T) {
	// 取消与周期结束并发到达：结果必须确定——取消生效则终止
	clock := NewManualClock(t0)
	e := newTestEngine(clock)
	sub, _ := e.Subscribe("acct1", "basic", "")

	clock.Set(t0.AddDate(0, 0, 29).Add(23*time.Hour + 59*time.Minute))
	if _, err := e.Cancel(sub.ID); err != nil {
		t.Fatal(err)
	}
	// 周期结束事件与取消并发：取消优先，订阅终止而非续费
	if err := e.Advance(t0.AddDate(0, 0, 30)); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusExpired {
		t.Fatalf("want expired, got %s", sub.Status)
	}
	if n := len(e.Invoices(sub.ID)); n != 1 {
		t.Fatalf("renewal happened despite cancel: invoices=%d", n)
	}
}
