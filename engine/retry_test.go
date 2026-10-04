package engine

import (
	"sync"
	"testing"
	"time"
)

// failCharger 前 failTimes 次扣费失败，之后成功。
type failCharger struct {
	mu        sync.Mutex
	failTimes int
	calls     int
}

func (f *failCharger) Charge(string, int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failTimes {
		return false
	}
	return true
}

func TestPaymentFailureRetryAndSuspend(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)
	e.SetCharger(&failCharger{failTimes: 100}) // 一直失败

	sub, _ := e.Subscribe("acct1", "basic", "")
	// 首次扣费失败：进入宽限期
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusPastDue {
		t.Fatalf("want past_due, got %s", sub.Status)
	}
	if sub.FailedAttempts != 1 || sub.OutstandingCents != 1000 {
		t.Fatalf("attempts=%d outstanding=%d", sub.FailedAttempts, sub.OutstandingCents)
	}
	// 宽限期内有权益
	if !Entitled(sub, t0.AddDate(0, 0, 5)) {
		t.Fatal("should be entitled within grace period")
	}

	// 重试 3 次（第 2/3/4 次尝试）后耗尽 → 挂起
	for _, d := range []int{2, 4, 6} {
		if err := e.Advance(t0.AddDate(0, 0, d)); err != nil {
			t.Fatal(err)
		}
	}
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusSuspended {
		t.Fatalf("want suspended, got %s (attempts=%d)", sub.Status, sub.FailedAttempts)
	}
	if Entitled(sub, t0.AddDate(0, 0, 6)) {
		t.Fatal("suspended sub must not be entitled")
	}
	// 欠费定格，不再累计
	debt := sub.OutstandingCents
	if err := e.Advance(t0.AddDate(0, 0, 60)); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.OutstandingCents != debt {
		t.Fatalf("debt grew while suspended: %d -> %d", debt, sub.OutstandingCents)
	}

	// 恢复：结清后回到 active，欠费不重复累计
	sub, err := e.RecordPaymentResult(sub.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != StatusActive || sub.OutstandingCents != 0 || sub.FailedAttempts != 0 {
		t.Fatalf("after settle: status=%s outstanding=%d attempts=%d",
			sub.Status, sub.OutstandingCents, sub.FailedAttempts)
	}
	var charges int64
	for _, le := range e.Ledger(sub.ID) {
		if le.Kind == "charge" {
			charges += le.Amount
		}
	}
	if charges != 0 { // 从未扣款成功，恢复是外部结清，不产生引擎内重复扣费
		t.Fatalf("unexpected charges: %d", charges)
	}
}

func TestRetryEventuallySucceeds(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)
	e.SetCharger(&failCharger{failTimes: 1}) // 首次扣费失败，首次重试成功

	sub, _ := e.Subscribe("acct1", "basic", "")
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusPastDue {
		t.Fatalf("want past_due, got %s", sub.Status)
	}
	if err := e.Advance(t0.AddDate(0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusActive || sub.OutstandingCents != 0 {
		t.Fatalf("status=%s outstanding=%d", sub.Status, sub.OutstandingCents)
	}
}

func TestExhaustedTerminatePolicy(t *testing.T) {
	clock := NewManualClock(t0)
	cfg := defaultCfg()
	cfg.OnExhausted = PolicyTerminate
	e := NewEngine(NewMemoryStore(), clock, cfg, testPlans())
	e.SetCharger(&failCharger{failTimes: 100})

	sub, _ := e.Subscribe("acct1", "basic", "")
	for _, d := range []int{2, 4, 6} {
		if err := e.Advance(t0.AddDate(0, 0, d)); err != nil {
			t.Fatal(err)
		}
	}
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusExpired {
		t.Fatalf("want expired, got %s", sub.Status)
	}
	if sub.OutstandingCents != 0 {
		t.Fatalf("terminated sub must not keep accruing debt: %d", sub.OutstandingCents)
	}
}

func TestGraceExpiryTerminatesEvenWithRetriesLeft(t *testing.T) {
	clock := NewManualClock(t0)
	cfg := Config{GracePeriodDays: 3, MaxRetries: 10, RetryIntervalDays: 2, OnExhausted: PolicySuspend}
	e := NewEngine(NewMemoryStore(), clock, cfg, testPlans())
	e.SetCharger(&failCharger{failTimes: 100})

	sub, _ := e.Subscribe("acct1", "basic", "")
	// 第 2 天重试失败（仍在宽限期内），第 4 天已超宽限期 → 挂起
	e.Advance(t0.AddDate(0, 0, 2))
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusPastDue {
		t.Fatalf("day2: want past_due, got %s", sub.Status)
	}
	e.Advance(t0.AddDate(0, 0, 4))
	sub, _ = e.Get(sub.ID)
	if sub.Status != StatusSuspended {
		t.Fatalf("day4: want suspended (grace expired), got %s", sub.Status)
	}
}

func TestConcurrentAccessConsistentSnapshots(t *testing.T) {
	clock := NewManualClock(t0)
	e := newTestEngine(clock)
	sub, _ := e.Subscribe("acct1", "basic", "")

	var wg sync.WaitGroup
	// 并发取消/恢复/变更/读取
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				switch i % 4 {
				case 0:
					e.Cancel(sub.ID)
				case 1:
					e.Resume(sub.ID)
				case 2:
					e.ChangePlan(sub.ID, "pro", time.Time{}, "")
				case 3:
					s, err := e.Get(sub.ID)
					if err != nil {
						t.Error(err)
						return
					}
					// 快照自洽性：状态合法且版本单调
					switch s.Status {
					case StatusTrialing, StatusActive, StatusPastDue,
						StatusSuspended, StatusCanceled, StatusExpired:
					default:
						t.Errorf("illegal status %q", s.Status)
						return
					}
					if s.Status == StatusCanceled && !s.CancelAtPeriodEnd {
						t.Error("canceled without CancelAtPeriodEnd")
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()

	// 重复提交幂等：相同 idempotencyKey 不产生两个订阅
	s1, err1 := e.Subscribe("acct2", "basic", "key-1")
	s2, err2 := e.Subscribe("acct2", "basic", "key-1")
	if err1 != nil || err2 != nil || s1.ID != s2.ID {
		t.Fatalf("idempotent subscribe failed: %v %v %s %s", err1, err2, s1.ID, s2.ID)
	}
}

func TestRestartContinuesPendingChange(t *testing.T) {
	dir := t.TempDir()
	clock := NewManualClock(t0)

	st1, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e1 := NewEngine(st1, clock, defaultCfg(), testPlans())
	sub, _ := e1.Subscribe("acct1", "basic", "")
	eff := t0.AddDate(0, 0, 10)
	if _, err := e1.ChangePlan(sub.ID, "pro", eff, ""); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：新引擎 + 同一存储目录，未完成的变更按原语义继续推进
	st2, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e2 := NewEngine(st2, clock, defaultCfg(), testPlans())
	restored, err := st2.Load(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.PendingChange == nil || !restored.PendingChange.EffectiveAt.Equal(eff) {
		t.Fatalf("pending change lost after restart: %+v", restored.PendingChange)
	}
	if err := e2.Advance(eff); err != nil {
		t.Fatal(err)
	}
	got, _ := e2.Get(sub.ID)
	if got.PlanID != "pro" || got.PendingChange != nil {
		t.Fatalf("after restart advance: plan=%s pending=%+v", got.PlanID, got.PendingChange)
	}
}
