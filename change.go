package subscription

import "time"

// ChangePlanRequest upgrades or downgrades the subscription.
// EffectiveAt zero (or past) means immediate; a future time schedules the
// change, which stays revocable until it takes effect.
type ChangePlanRequest struct {
	RequestID   string
	AccountID   string
	TargetPlan  string
	EffectiveAt time.Time
}

// ChangePlan applies or schedules a plan change. Idempotent by RequestID.
func (e *Engine) ChangePlan(req ChangePlanRequest) (Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dup, err := e.checkIdempotent(req.RequestID, "change:"+req.AccountID+":"+req.TargetPlan); err != nil {
		return Subscription{}, err
	} else if dup {
		return e.currentLocked(req.AccountID)
	}
	s, ok := e.subs[req.AccountID]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	if s.Status == StatusEnded {
		return Subscription{}, ErrSubscriptionEnded
	}
	target, ok := e.catalog.Plan(req.TargetPlan)
	if !ok {
		return Subscription{}, ErrPlanNotFound
	}
	if target.ID == s.PlanID {
		return Subscription{}, ErrInvalidState
	}
	kind := ChangeUpgrade
	if target.Rank < mustRank(e.catalog, s.PlanID) {
		kind = ChangeDowngrade
	}
	now := e.clock.Now()
	if req.EffectiveAt.After(now) {
		s.PendingChange = &ScheduledChange{
			ID:           e.nextID("chg"),
			TargetPlanID: target.ID,
			Kind:         kind,
			IssuedAt:     now,
			EffectiveAt:  req.EffectiveAt,
		}
		s.Version++
		if err := e.persist(s); err != nil {
			return Subscription{}, err
		}
		return *cloneSubscription(s), nil
	}
	if err := e.switchPlan(s, target.ID, now); err != nil {
		return Subscription{}, err
	}
	return *cloneSubscription(s), nil
}

// switchPlan moves the subscription to a new plan at time `at`, prorating the
// remainder of the current period. A positive delta is billed immediately;
// a negative delta becomes carry-over credit for future periods.
func (e *Engine) switchPlan(s *Subscription, targetPlanID string, at time.Time) error {
	if s.Status == StatusTrialing {
		// No money has moved yet; the trial simply continues on the new plan.
		s.PlanID = targetPlanID
		s.Version++
		return e.persist(s)
	}
	oldPrice, err := e.catalog.PriceAt(s.PlanID, s.CurrentPeriodStart)
	if err != nil {
		return err
	}
	newPrice, err := e.catalog.PriceAt(targetPlanID, at)
	if err != nil {
		return err
	}
	s.PlanID = targetPlanID
	s.Version++
	return e.billSwitch(s, oldPrice, newPrice, at)
}

// billSwitch computes proration between the old and new price over the rest
// of the period, then settles the delta: charge now if positive, carry as
// credit if negative. The invoice lines keep both halves of the computation
// so the result reconciles with a per-cycle line-by-line recomputation.
func (e *Engine) billSwitch(s *Subscription, oldPrice, newPrice PriceVersion, at time.Time) error {
	pr := prorate(oldPrice, newPrice, s.CurrentPeriodStart, s.CurrentPeriodEnd, at)
	if pr.DeltaCents < 0 {
		s.CreditCents += -pr.DeltaCents
		return e.persist(s)
	}
	if pr.DeltaCents == 0 {
		return e.persist(s)
	}
	inv := &Invoice{
		ID:             e.nextID("inv"),
		SubscriptionID: s.ID,
		AccountID:      s.AccountID,
		Status:         InvoiceOpen,
		IssuedAt:       e.clock.Now(),
		Lines: []InvoiceLine{
			{
				Kind:        LineProration,
				Description: newPrice.ID,
				AmountCents: pr.DueCents,
				PeriodStart: at,
				PeriodEnd:   s.CurrentPeriodEnd,
			},
			{
				Kind:        LineProration,
				Description: oldPrice.ID,
				AmountCents: -pr.UnusedCents,
				PeriodStart: at,
				PeriodEnd:   s.CurrentPeriodEnd,
			},
		},
		AmountCents: pr.DeltaCents,
	}
	inv.CreditApplied = min64(s.CreditCents, inv.AmountCents)
	s.CreditCents -= inv.CreditApplied
	inv.NetCents = inv.AmountCents - inv.CreditApplied
	e.invoices[inv.ID] = inv
	if err := e.charge(s, inv, at); err != nil {
		return err
	}
	return e.persist(s, inv)
}

// CancelScheduledChangeRequest revokes a future-dated plan change.
type CancelScheduledChangeRequest struct {
	RequestID string
	AccountID string
}

// CancelScheduledChange removes a pending future change. Because a pending
// change never touches the current plan, clearing it returns the account to
// its original plan with no intermediate state left behind.
func (e *Engine) CancelScheduledChange(req CancelScheduledChangeRequest) (Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dup, err := e.checkIdempotent(req.RequestID, "cancelchange:"+req.AccountID); err != nil {
		return Subscription{}, err
	} else if dup {
		return e.currentLocked(req.AccountID)
	}
	s, ok := e.subs[req.AccountID]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	if s.PendingChange == nil {
		return Subscription{}, ErrNoPendingChange
	}
	if !e.clock.Now().Before(s.PendingChange.EffectiveAt) {
		return Subscription{}, ErrChangeNotCancellable
	}
	s.PendingChange = nil
	s.Version++
	if err := e.persist(s); err != nil {
		return Subscription{}, err
	}
	return *cloneSubscription(s), nil
}

// applyScheduledChange applies a due scheduled change. Billing uses the
// change's EffectiveAt (not the processing time), so a change issued before
// a restart keeps its original semantics when it is eventually applied.
func (e *Engine) applyScheduledChange(s *Subscription, now time.Time) error {
	pc := s.PendingChange
	s.PendingChange = nil
	return e.switchPlan(s, pc.TargetPlanID, pc.EffectiveAt)
}

func mustRank(c *Catalog, planID string) int {
	p, _ := c.Plan(planID)
	return p.Rank
}
