package engine

import (
	"fmt"
	"sync"
	"time"
)

// Charger 扣费通道，由调用方注入；返回是否扣款成功。
type Charger interface {
	Charge(subID string, amountCents int64) bool
}

type alwaysOKCharger struct{}

func (alwaysOKCharger) Charge(string, int64) bool { return true }

// Engine 订阅生命周期引擎。所有方法并发安全，读取返回快照拷贝。
type Engine struct {
	mu      sync.Mutex
	store   Store
	clock   Clock
	cfg     Config
	plans   map[string]Plan
	charger Charger
	ledger  map[string][]LedgerEntry // subID -> entries
	inv     map[string][]Invoice     // subID -> invoices
	idem    map[string]string        // idempotencyKey -> subID
	seq     int64
}

func NewEngine(store Store, clock Clock, cfg Config, plans []Plan) *Engine {
	m := make(map[string]Plan, len(plans))
	for _, p := range plans {
		m[p.ID] = p
	}
	return &Engine{
		store:   store,
		clock:   clock,
		cfg:     cfg,
		plans:   m,
		charger: alwaysOKCharger{},
		ledger:  make(map[string][]LedgerEntry),
		inv:     make(map[string][]Invoice),
		idem:    make(map[string]string),
	}
}

// SetCharger 注入扣费通道（默认全部成功）。
func (e *Engine) SetCharger(c Charger) { e.mu.Lock(); e.charger = c; e.mu.Unlock() }

func (e *Engine) nextID(prefix string) string {
	e.seq++
	return fmt.Sprintf("%s-%d", prefix, e.seq)
}

func (e *Engine) plan(id string) (Plan, error) {
	p, ok := e.plans[id]
	if !ok {
		return Plan{}, ErrPlanNotFound
	}
	return p, nil
}

// Subscribe 创建订阅（若套餐含试用则进入试用态）。idempotencyKey 防重复提交。
func (e *Engine) Subscribe(accountID, planID, idempotencyKey string) (*Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if idempotencyKey != "" {
		if id, ok := e.idem[idempotencyKey]; ok {
			return e.getLocked(id)
		}
	}
	p, err := e.plan(planID)
	if err != nil {
		return nil, err
	}
	now := e.clock.Now()
	sub := &Subscription{
		ID:                 e.nextID("sub"),
		AccountID:          accountID,
		PlanID:             planID,
		CurrentPeriodStart: now,
	}
	if p.TrialDays > 0 {
		sub.Status = StatusTrialing
		sub.TrialEnd = now.AddDate(0, 0, p.TrialDays)
		sub.CurrentPeriodEnd = sub.TrialEnd
	} else {
		sub.Status = StatusActive
		sub.CurrentPeriodEnd = now.AddDate(0, 0, p.IntervalDays)
		e.billLocked(sub, p.PriceCents, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, "initial")
	}
	sub.Version++
	if err := e.store.Save(sub); err != nil {
		return nil, err
	}
	if idempotencyKey != "" {
		e.idem[idempotencyKey] = sub.ID
	}
	cp := *sub
	return &cp, nil
}

// ChangePlan 升级或降级。effectiveAt 为零值表示立即生效，否则未来生效。
func (e *Engine) ChangePlan(subID, toPlanID string, effectiveAt time.Time, idempotencyKey string) (*Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if idempotencyKey != "" {
		if _, ok := e.idem[idempotencyKey]; ok {
			return e.getLocked(subID) // 重复提交：直接返回当前快照
		}
	}
	sub, err := e.getLocked(subID)
	if err != nil {
		return nil, err
	}
	newPlan, err := e.plan(toPlanID)
	if err != nil {
		return nil, err
	}
	oldPlan, err := e.plan(sub.PlanID)
	if err != nil {
		return nil, err
	}
	switch sub.Status {
	case StatusActive, StatusPastDue, StatusCanceled:
	default:
		return nil, ErrInvalidState
	}
	kind := ChangeUpgrade
	if newPlan.PriceCents < oldPlan.PriceCents {
		kind = ChangeDowngrade
	}
	now := e.clock.Now()
	if effectiveAt.IsZero() || !effectiveAt.After(now) {
		e.applyChangeLocked(sub, oldPlan, newPlan, now)
	} else {
		if sub.PendingChange != nil {
			return nil, ErrChangeAlreadyExists
		}
		sub.PendingChange = &PendingChange{
			ID:          e.nextID("chg"),
			Kind:        kind,
			ToPlanID:    toPlanID,
			EffectiveAt: effectiveAt,
			CreatedAt:   now,
		}
	}
	sub.Version++
	if idempotencyKey != "" {
		e.idem[idempotencyKey] = sub.ID
	}
	if err := e.store.Save(sub); err != nil {
		return nil, err
	}
	cp := *sub
	return &cp, nil
}

// applyChangeLocked 立即按 now 时刻的剩余时长折算补差/结余并切换套餐。
func (e *Engine) applyChangeLocked(sub *Subscription, oldPlan, newPlan Plan, at time.Time) {
	delta := proration(oldPlan, newPlan, at, sub.CurrentPeriodStart, sub.CurrentPeriodEnd)
	if delta > 0 {
		e.addLedger(sub.ID, "proration_debit", delta, fmt.Sprintf("%s->%s", oldPlan.ID, newPlan.ID))
		sub.OutstandingCents += delta
		if e.charger.Charge(sub.ID, delta) {
			sub.OutstandingCents -= delta
			e.addLedger(sub.ID, "charge", delta, "proration paid")
		}
	} else if delta < 0 {
		sub.CreditCents += -delta
		e.addLedger(sub.ID, "proration_credit", -delta, fmt.Sprintf("%s->%s", oldPlan.ID, newPlan.ID))
	}
	sub.PlanID = newPlan.ID
	sub.PendingChange = nil
}

// CancelPendingChange 撤销未来生效的变更，回到原套餐，不残留中间状态。
func (e *Engine) CancelPendingChange(subID string) (*Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sub, err := e.getLocked(subID)
	if err != nil {
		return nil, err
	}
	if sub.PendingChange == nil {
		return nil, ErrNoPendingChange
	}
	sub.PendingChange = nil
	sub.Version++
	if err := e.store.Save(sub); err != nil {
		return nil, err
	}
	cp := *sub
	return &cp, nil
}

// Cancel 取消订阅，周期结束时终止；期间权益不变。
func (e *Engine) Cancel(subID string) (*Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sub, err := e.getLocked(subID)
	if err != nil {
		return nil, err
	}
	switch sub.Status {
	case StatusActive, StatusPastDue, StatusTrialing:
	default:
		return nil, ErrInvalidState
	}
	if sub.CancelAtPeriodEnd {
		cp := *sub // 幂等：重复取消不产生二次迁移
		return &cp, nil
	}
	sub.CancelAtPeriodEnd = true
	sub.Status = StatusCanceled
	sub.Version++
	if err := e.store.Save(sub); err != nil {
		return nil, err
	}
	cp := *sub
	return &cp, nil
}

// Resume 在周期结束前撤销取消。
func (e *Engine) Resume(subID string) (*Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sub, err := e.getLocked(subID)
	if err != nil {
		return nil, err
	}
	if sub.Status != StatusCanceled {
		return nil, ErrInvalidState
	}
	sub.CancelAtPeriodEnd = false
	sub.Status = StatusActive
	sub.Version++
	if err := e.store.Save(sub); err != nil {
		return nil, err
	}
	cp := *sub
	return &cp, nil
}

// RecordPaymentResult 记录外部扣费结果（如用户补缴欠费）。
func (e *Engine) RecordPaymentResult(subID string, success bool) (*Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sub, err := e.getLocked(subID)
	if err != nil {
		return nil, err
	}
	if success {
		e.settleLocked(sub)
	} else {
		e.failLocked(sub, e.clock.Now())
	}
	sub.Version++
	if err := e.store.Save(sub); err != nil {
		return nil, err
	}
	cp := *sub
	return &cp, nil
}

// settleLocked 欠费结清：恢复 active，不重复累计欠费。
func (e *Engine) settleLocked(sub *Subscription) {
	sub.OutstandingCents = 0
	sub.FailedAttempts = 0
	sub.NextRetryAt = time.Time{}
	sub.GraceUntil = time.Time{}
	invs := e.inv[sub.ID]
	for i := range invs {
		invs[i].Paid = true
	}
	switch sub.Status {
	case StatusPastDue:
		sub.Status = StatusActive
	case StatusSuspended:
		// 恢复：周期从当前时刻重新开始，历史欠费已结清，不再累计
		now := e.clock.Now()
		p := e.plans[sub.PlanID]
		sub.Status = StatusActive
		sub.CurrentPeriodStart = now
		sub.CurrentPeriodEnd = now.AddDate(0, 0, p.IntervalDays)
	}
}

// failLocked 扣费失败：进入宽限期并安排重试；耗尽后按策略终止或挂起。
func (e *Engine) failLocked(sub *Subscription, at time.Time) {
	sub.FailedAttempts++
	if sub.GraceUntil.IsZero() {
		sub.GraceUntil = at.AddDate(0, 0, e.cfg.GracePeriodDays)
	}
	if sub.FailedAttempts > e.cfg.MaxRetries || !at.Before(sub.GraceUntil) {
		if e.cfg.OnExhausted == PolicySuspend {
			sub.Status = StatusSuspended
		} else {
			sub.Status = StatusExpired
			sub.OutstandingCents = 0 // 终止后不再累计欠费
		}
		sub.NextRetryAt = time.Time{}
		return
	}
	sub.Status = StatusPastDue
	sub.NextRetryAt = at.AddDate(0, 0, e.cfg.RetryIntervalDays)
}

// Advance 将引擎推进到时刻 t：处理试用到期、周期续费、未来变更生效、重试。
// 幂等：同一时刻重复调用不产生重复迁移（事件处理后即被消费）。
func (e *Engine) Advance(t time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	subs, err := e.store.List()
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if e.processLocked(sub, t) {
			if err := e.store.Save(sub); err != nil {
				return err
			}
		}
	}
	return nil
}

// processLocked 按时间顺序消费 sub 上所有 <= t 的事件，返回是否有迁移。
func (e *Engine) processLocked(sub *Subscription, t time.Time) bool {
	changed := false
	for {
		ev, at := e.nextEvent(sub)
		if ev == "" || at.After(t) {
			return changed
		}
		e.applyEvent(sub, ev, at)
		sub.Version++
		changed = true
	}
}

func (e *Engine) nextEvent(sub *Subscription) (string, time.Time) {
	var ev string
	var at time.Time
	consider := func(name string, ts time.Time) {
		if ts.IsZero() {
			return
		}
		if ev == "" || ts.Before(at) {
			ev, at = name, ts
		}
	}
	if sub.Status == StatusTrialing {
		consider("trial_end", sub.TrialEnd)
	}
	if sub.Status == StatusActive || sub.Status == StatusCanceled || sub.Status == StatusPastDue {
		consider("period_end", sub.CurrentPeriodEnd)
	}
	if sub.PendingChange != nil && (sub.Status == StatusActive || sub.Status == StatusPastDue || sub.Status == StatusCanceled) {
		consider("pending_change", sub.PendingChange.EffectiveAt)
	}
	if sub.Status == StatusPastDue && !sub.NextRetryAt.IsZero() {
		consider("retry", sub.NextRetryAt)
	}
	return ev, at
}

func (e *Engine) applyEvent(sub *Subscription, ev string, at time.Time) {
	switch ev {
	case "trial_end":
		p := e.plans[sub.PlanID]
		sub.Status = StatusActive
		sub.CurrentPeriodStart = at
		sub.CurrentPeriodEnd = at.AddDate(0, 0, p.IntervalDays)
		e.billLocked(sub, p.PriceCents, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, "trial_conversion")
	case "period_end":
		if sub.CancelAtPeriodEnd {
			sub.Status = StatusExpired
			sub.PendingChange = nil
			return
		}
		p := e.plans[sub.PlanID]
		sub.CurrentPeriodStart = at
		sub.CurrentPeriodEnd = at.AddDate(0, 0, p.IntervalDays)
		e.billLocked(sub, p.PriceCents, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, "renewal")
	case "pending_change":
		pc := sub.PendingChange
		oldPlan := e.plans[sub.PlanID]
		newPlan := e.plans[pc.ToPlanID]
		e.applyChangeLocked(sub, oldPlan, newPlan, at)
	case "retry":
		amt := sub.OutstandingCents
		if e.charger.Charge(sub.ID, amt) {
			e.settleLocked(sub)
			e.addLedger(sub.ID, "charge", amt, "retry success")
		} else {
			e.failLocked(sub, at)
		}
	}
}

// billLocked 出账：结余抵扣后尝试扣费，失败则进入宽限/重试流程。
func (e *Engine) billLocked(sub *Subscription, amount int64, start, end time.Time, reason string) {
	charge, credit := applyCredit(amount, sub.CreditCents)
	sub.CreditCents = credit
	if amount-charge > 0 {
		e.addLedger(sub.ID, "credit_applied", amount-charge, reason)
	}
	inv := Invoice{
		ID:             e.nextID("inv"),
		SubscriptionID: sub.ID,
		PeriodStart:    start,
		PeriodEnd:      end,
		AmountCents:    charge,
		CreatedAt:      e.clock.Now(),
	}
	if charge > 0 {
		if e.charger.Charge(sub.ID, charge) {
			inv.Paid = true
			e.addLedger(sub.ID, "charge", charge, reason)
		} else {
			sub.OutstandingCents += charge
			e.failLocked(sub, e.clock.Now())
		}
	} else {
		inv.Paid = true
	}
	e.inv[sub.ID] = append(e.inv[sub.ID], inv)
}

func (e *Engine) addLedger(subID, kind string, amount int64, reason string) {
	e.ledger[subID] = append(e.ledger[subID], LedgerEntry{
		ID:        e.nextID("led"),
		Kind:      kind,
		Amount:    amount,
		Reason:    reason,
		CreatedAt: e.clock.Now(),
	})
}

// Get 读取订阅快照（拷贝，任意时刻都是自洽状态）。
func (e *Engine) Get(subID string) (*Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.getLocked(subID)
}

func (e *Engine) getLocked(subID string) (*Subscription, error) {
	sub, err := e.store.Load(subID)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// Ledger 返回账本（拷贝）。
func (e *Engine) Ledger(subID string) []LedgerEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]LedgerEntry, len(e.ledger[subID]))
	copy(out, e.ledger[subID])
	return out
}

// Invoices 返回账单（拷贝）。
func (e *Engine) Invoices(subID string) []Invoice {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Invoice, len(e.inv[subID]))
	copy(out, e.inv[subID])
	return out
}

// Entitled 判断某时刻订阅是否享有权益（口径：试用/正常/已取消未到期的均有权益；
// 宽限期内有权益；挂起与终止无权益）。
func Entitled(sub *Subscription, t time.Time) bool {
	switch sub.Status {
	case StatusTrialing, StatusActive:
		return true
	case StatusCanceled:
		return t.Before(sub.CurrentPeriodEnd)
	case StatusPastDue:
		return sub.GraceUntil.IsZero() || !t.After(sub.GraceUntil)
	default:
		return false
	}
}
