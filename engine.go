package subscription

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// PaymentGateway charges an account; injected so tests can simulate failures.
type PaymentGateway interface {
	Charge(accountID string, amountCents int64, invoiceID string) error
}

// Engine is the subscription lifecycle engine. All mutations are serialized
// through a single mutex, so any read observes a self-consistent snapshot:
// plan, status, entitlements and billing never appear half-applied.
type Engine struct {
	mu       sync.Mutex
	clock    Clock
	catalog  *Catalog
	cfg      Config
	gateway  PaymentGateway
	store    Store
	subs     map[string]*Subscription // by account ID
	invoices map[string]*Invoice
	reqLog   map[string]string // requestID -> "op:account" for idempotency
	seq      int64
}

// NewEngine creates an engine, reloading persisted state if the store has any.
// Pending scheduled changes keep the EffectiveAt they were issued with, so a
// restart continues them with their original semantics.
func NewEngine(clock Clock, catalog *Catalog, cfg Config, gw PaymentGateway, store Store) (*Engine, error) {
	e := &Engine{
		clock:    clock,
		catalog:  catalog,
		cfg:      cfg,
		gateway:  gw,
		store:    store,
		subs:     make(map[string]*Subscription),
		invoices: make(map[string]*Invoice),
		reqLog:   make(map[string]string),
	}
	if store != nil {
		subs, invs, err := store.Load()
		if err != nil {
			return nil, err
		}
		for _, s := range subs {
			e.subs[s.AccountID] = s
		}
		for _, inv := range invs {
			e.invoices[inv.ID] = inv
		}
	}
	return e, nil
}

// checkIdempotent reports whether reqID was already applied. The first call
// records (op, accountID); a repeat with the same key is a no-op duplicate,
// a repeat with a different key is an error.
func (e *Engine) checkIdempotent(reqID, key string) (dup bool, err error) {
	if reqID == "" {
		return false, nil
	}
	prev, ok := e.reqLog[reqID]
	if !ok {
		e.reqLog[reqID] = key
		return false, nil
	}
	if prev != key {
		return false, fmt.Errorf("%w: request %s already used for %s", ErrDuplicateRequest, reqID, prev)
	}
	return true, nil
}

// GetSubscription returns a consistent snapshot copy for an account.
func (e *Engine) GetSubscription(accountID string) (Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.subs[accountID]
	if !ok {
		return Subscription{}, ErrNotFound
	}
	return *cloneSubscription(s), nil
}

// ListInvoices returns invoice snapshots for an account in issuance order.
func (e *Engine) ListInvoices(accountID string) []Invoice {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Invoice
	for _, inv := range e.invoices {
		if inv.AccountID == accountID {
			out = append(out, *cloneInvoice(inv))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].IssuedAt.Equal(out[j].IssuedAt) {
			return out[i].IssuedAt.Before(out[j].IssuedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ProcessDue advances all time-based transitions due at the current clock
// time, in a deterministic order per subscription:
//  1. trial expiry (auto-conversion),
//  2. due scheduled plan changes,
//  3. period-end: cancellation or renewal,
//  4. payment retries and grace-period expiry.
func (e *Engine) ProcessDue() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	for _, s := range e.subs {
		if s.Status == StatusEnded {
			continue
		}
		if s.Status == StatusTrialing && s.TrialEnd != nil && !now.Before(*s.TrialEnd) {
			if err := e.expireTrial(s, now); err != nil {
				return err
			}
		}
		if s.PendingChange != nil && !now.Before(s.PendingChange.EffectiveAt) {
			if err := e.applyScheduledChange(s, now); err != nil {
				return err
			}
		}
		if !now.Before(s.CurrentPeriodEnd) {
			switch {
			case s.CancelAtPeriodEnd:
				if err := e.endAtPeriodEnd(s, now); err != nil {
					return err
				}
			case s.Status == StatusActive:
				if err := e.renew(s, now); err != nil {
					return err
				}
			}
		}
	}
	return e.retryDue(now)
}

// renew closes the current period and opens the next, billing at the price
// effective at renewal time. Implemented in billing.go.
func (e *Engine) renew(s *Subscription, now time.Time) error {
	price, err := e.catalog.PriceAt(s.PlanID, now)
	if err != nil {
		return err
	}
	inv := e.newInvoice(s, s.CurrentPeriodEnd, s.CurrentPeriodEnd.AddDate(0, 0, price.IntervalDays), price)
	s.CurrentPeriodStart = inv.Lines[0].PeriodStart
	s.CurrentPeriodEnd = inv.Lines[0].PeriodEnd
	s.Version++
	if err := e.charge(s, inv, now); err != nil {
		return err
	}
	return e.persist(s, inv)
}

// persist writes current state to the store (best-effort durability).
func (e *Engine) persist(s *Subscription, invs ...*Invoice) error {
	if e.store == nil {
		return nil
	}
	if s != nil {
		if err := e.store.SaveSubscription(s); err != nil {
			return err
		}
	}
	for _, inv := range invs {
		if inv != nil {
			if err := e.store.SaveInvoice(inv); err != nil {
				return err
			}
		}
	}
	return nil
}

// nextID generates a unique id with a human-readable prefix.
func (e *Engine) nextID(prefix string) string {
	e.seq++
	return fmt.Sprintf("%s-%d", prefix, e.seq)
}
