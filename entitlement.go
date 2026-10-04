package subscription

// Entitlement is the entitlement verdict for an account at read time.
type Entitlement struct {
	PlanID   string
	Entitled bool
	Reason   string
}

// Entitlement reports whether the account currently has access, and to which
// plan. The rule is fixed: trialing, active and past_due (inside the grace
// window) are entitled; suspended and ended are not.
func (e *Engine) Entitlement(accountID string) (Entitlement, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.subs[accountID]
	if !ok {
		return Entitlement{}, ErrNotFound
	}
	ent := Entitlement{PlanID: s.PlanID}
	switch s.Status {
	case StatusTrialing, StatusActive:
		ent.Entitled = true
		ent.Reason = string(s.Status)
	case StatusPastDue:
		ent.Entitled = true
		ent.Reason = "past_due: within grace period, entitlement retained"
	case StatusSuspended:
		ent.Reason = "suspended: retries and grace exhausted"
	case StatusEnded:
		ent.Reason = "ended"
	}
	return ent, nil
}
