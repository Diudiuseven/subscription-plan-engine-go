package subscription

import "time"

// newInvoice builds an invoice for one full period at the given price version.
// The price is snapshotted into the line at issuance, so later price changes
// never alter already-issued invoices. Carry-over credit is consumed here.
func (e *Engine) newInvoice(s *Subscription, start, end time.Time, price PriceVersion) *Invoice {
	inv := &Invoice{
		ID:             e.nextID("inv"),
		SubscriptionID: s.ID,
		AccountID:      s.AccountID,
		Status:         InvoiceOpen,
		IssuedAt:       e.clock.Now(),
		Lines: []InvoiceLine{{
			Kind:        LineSubscription,
			Description: price.ID,
			AmountCents: price.AmountCents,
			PeriodStart: start,
			PeriodEnd:   end,
		}},
		AmountCents: price.AmountCents,
	}
	inv.CreditApplied = min64(s.CreditCents, inv.AmountCents)
	s.CreditCents -= inv.CreditApplied
	inv.NetCents = inv.AmountCents - inv.CreditApplied
	e.invoices[inv.ID] = inv
	return inv
}

// charge attempts payment for an invoice. On failure it arms the retry
// schedule and the grace window; the subscription keeps full entitlement
// (past_due) until retries and grace are exhausted.
func (e *Engine) charge(s *Subscription, inv *Invoice, now time.Time) error {
	if inv.NetCents <= 0 {
		inv.Status = InvoicePaid
		return nil
	}
	if err := e.gateway.Charge(s.AccountID, inv.NetCents, inv.ID); err != nil {
		inv.Attempts = 1
		inv.GraceUntil = now.Add(e.cfg.GracePeriod)
		e.armRetry(inv, now)
		if s.Status == StatusActive {
			s.Status = StatusPastDue
			s.Version++
		}
		return nil
	}
	inv.Status = InvoicePaid
	return nil
}

// armRetry sets NextAttemptAt for the next retry, or for the grace deadline
// once the configured retries are used up (a final attempt at grace end).
func (e *Engine) armRetry(inv *Invoice, now time.Time) {
	if inv.Attempts <= len(e.cfg.Retry.Intervals) {
		inv.NextAttemptAt = now.Add(e.cfg.Retry.Intervals[inv.Attempts-1])
		return
	}
	inv.NextAttemptAt = inv.GraceUntil
}

// retryDue processes invoices whose retry time has arrived and applies the
// exhausted-action policy once retries and grace are spent.
func (e *Engine) retryDue(now time.Time) error {
	for _, inv := range e.invoices {
		if inv.Status != InvoiceOpen || now.Before(inv.NextAttemptAt) {
			continue
		}
		s := e.subs[inv.AccountID]
		if s == nil || s.Status == StatusEnded {
			continue
		}
		if err := e.gateway.Charge(inv.AccountID, inv.NetCents, inv.ID); err != nil {
			inv.Attempts++
			if inv.Attempts <= len(e.cfg.Retry.Intervals) {
				e.armRetry(inv, now)
			} else if !now.Before(inv.GraceUntil) {
				e.applyExhausted(s, inv, now)
			} else {
				inv.NextAttemptAt = inv.GraceUntil
			}
		} else {
			inv.Status = InvoicePaid
			e.recoverPastDue(s, now)
		}
		if err := e.persist(s, inv); err != nil {
			return err
		}
	}
	return nil
}

// applyExhausted ends the grace window per configuration: suspend (recoverable,
// no entitlement, the debt stays open and keeps being retried on the last
// retry interval) or terminate (final, the invoice is written off).
func (e *Engine) applyExhausted(s *Subscription, inv *Invoice, now time.Time) {
	switch e.cfg.OnRetryExhausted {
	case ActionTerminate:
		inv.Status = InvoiceFailed
		s.Status = StatusEnded
	default:
		s.Status = StatusSuspended
		interval := 24 * time.Hour
		if n := len(e.cfg.Retry.Intervals); n > 0 {
			interval = e.cfg.Retry.Intervals[n-1]
		}
		inv.NextAttemptAt = now.Add(interval)
	}
	s.Version++
}

// recoverPastDue restores a subscription to active after its invoices are
// paid. Recovery never re-bills already-issued invoices, so arrears are not
// counted twice; if the period rolled over while suspended, one renewal
// realigns the cycle.
func (e *Engine) recoverPastDue(s *Subscription, now time.Time) error {
	if s.Status != StatusPastDue && s.Status != StatusSuspended {
		return nil
	}
	for _, inv := range e.invoices {
		if inv.AccountID == s.AccountID && inv.Status == InvoiceOpen {
			return nil // still owes; stay in dunning state
		}
	}
	s.Status = StatusActive
	s.Version++
	if !now.Before(s.CurrentPeriodEnd) {
		return e.renew(s, now)
	}
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
