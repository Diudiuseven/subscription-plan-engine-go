package subscription

import "time"

// StartTrialRequest starts a trial subscription for an account.
type StartTrialRequest struct {
	RequestID string
	AccountID string
	PlanID    string
}

// StartTrial creates a trialing subscription. Idempotent by RequestID:
// a duplicate returns the current state without a second state migration.
func (e *Engine) StartTrial(req StartTrialRequest) (Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dup, err := e.checkIdempotent(req.RequestID, "trial:"+req.AccountID); err != nil {
		return Subscription{}, err
	} else if dup {
		return e.currentLocked(req.AccountID)
	}
	if _, exists := e.subs[req.AccountID]; exists {
		return Subscription{}, ErrAlreadyExists
	}
	plan, ok := e.catalog.Plan(req.PlanID)
	if !ok {
		return Subscription{}, ErrPlanNotFound
	}
	now := e.clock.Now()
	trialEnd := now.AddDate(0, 0, plan.TrialDays)
	s := &Subscription{
		ID:                 e.nextID("sub"),
		AccountID:          req.AccountID,
		PlanID:             plan.ID,
		Status:             StatusTrialing,
		CurrentPeriodStart: now,
		CurrentPeriodEnd:   trialEnd,
		TrialEnd:           &trialEnd,
		Version:            1,
	}
	e.subs[req.AccountID] = s
	if err := e.persist(s); err != nil {
		return Subscription{}, err
	}
	return *cloneSubscription(s), nil
}

// ConvertTrialRequest converts a trial to a paid subscription.
type ConvertTrialRequest struct {
	RequestID string
	AccountID string
}

// ConvertTrial converts a trialing subscription to active. The paid period
// starts at the conversion time (or at the trial end if the trial already
// lapsed), so the transition happens at one deterministic instant.
// Idempotent by RequestID.
func (e *Engine) ConvertTrial(req ConvertTrialRequest) (Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dup, err := e.checkIdempotent(req.RequestID, "convert:"+req.AccountID); err != nil {
		return Subscription{}, err
	} else if dup {
		return e.currentLocked(req.AccountID)
	}
	s, ok := e.subs[req.AccountID]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	if s.Status != StatusTrialing {
		return Subscription{}, ErrInvalidState
	}
	now := e.clock.Now()
	start := now
	if s.TrialEnd != nil && now.After(*s.TrialEnd) {
		start = *s.TrialEnd
	}
	if err := e.startPaidPeriod(s, start, now); err != nil {
		return Subscription{}, err
	}
	return *cloneSubscription(s), nil
}

// expireTrial auto-converts a trialing subscription whose trial has ended.
// Called from ProcessDue; the status flip makes repeat triggers no-ops.
func (e *Engine) expireTrial(s *Subscription, now time.Time) error {
	return e.startPaidPeriod(s, *s.TrialEnd, now)
}

// startPaidPeriod flips a trial into the first paid period and bills it.
func (e *Engine) startPaidPeriod(s *Subscription, periodStart, now time.Time) error {
	price, err := e.catalog.PriceAt(s.PlanID, periodStart)
	if err != nil {
		return err
	}
	s.Status = StatusActive
	s.TrialEnd = nil
	inv := e.newInvoice(s, periodStart, periodStart.AddDate(0, 0, price.IntervalDays), price)
	s.CurrentPeriodStart = inv.Lines[0].PeriodStart
	s.CurrentPeriodEnd = inv.Lines[0].PeriodEnd
	s.Version++
	if err := e.charge(s, inv, now); err != nil {
		return err
	}
	return e.persist(s, inv)
}

// currentLocked returns the snapshot for an account (caller holds the lock).
func (e *Engine) currentLocked(accountID string) (Subscription, error) {
	s, ok := e.subs[accountID]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	return *cloneSubscription(s), nil
}
