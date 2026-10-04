package subscription

import "time"

// CancelRequest cancels a subscription at the end of the current period.
type CancelRequest struct {
	RequestID string
	AccountID string
}

// Cancel marks the subscription to end when the current period closes.
// Until then it keeps its status and full entitlement. Idempotent: repeated
// cancels return the same state. A renewal and a cancel racing at the period
// boundary are decided deterministically — ProcessDue checks the cancel flag
// before renewing, so a canceled subscription never renews.
func (e *Engine) Cancel(req CancelRequest) (Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dup, err := e.checkIdempotent(req.RequestID, "cancel:"+req.AccountID); err != nil {
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
	s.CancelAtPeriodEnd = true
	s.Version++
	if err := e.persist(s); err != nil {
		return Subscription{}, err
	}
	return *cloneSubscription(s), nil
}

// ResumeRequest revokes a pending cancellation.
type ResumeRequest struct {
	RequestID string
	AccountID string
}

// Resume clears the cancel-at-period-end flag. Once the period has ended and
// the subscription terminated, resuming is no longer possible.
func (e *Engine) Resume(req ResumeRequest) (Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dup, err := e.checkIdempotent(req.RequestID, "resume:"+req.AccountID); err != nil {
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
	if !s.CancelAtPeriodEnd {
		return Subscription{}, ErrInvalidState
	}
	s.CancelAtPeriodEnd = false
	s.Version++
	if err := e.persist(s); err != nil {
		return Subscription{}, err
	}
	return *cloneSubscription(s), nil
}

// endAtPeriodEnd terminates a subscription whose cancellation took effect.
// Any pending scheduled change dies with the subscription.
func (e *Engine) endAtPeriodEnd(s *Subscription, now time.Time) error {
	s.Status = StatusEnded
	s.CancelAtPeriodEnd = false
	s.PendingChange = nil
	s.Version++
	return e.persist(s)
}
