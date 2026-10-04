package subscription

import "sync"

// Store persists engine state so a restart can resume pending work with the
// semantics in force when each change was issued.
type Store interface {
	SaveSubscription(s *Subscription) error
	SaveInvoice(inv *Invoice) error
	Load() (subs []*Subscription, invoices []*Invoice, err error)
}

// cloneSubscription deep-copies a subscription.
func cloneSubscription(s *Subscription) *Subscription {
	if s == nil {
		return nil
	}
	out := *s
	if s.TrialEnd != nil {
		t := *s.TrialEnd
		out.TrialEnd = &t
	}
	if s.PendingChange != nil {
		pc := *s.PendingChange
		out.PendingChange = &pc
	}
	return &out
}

// cloneInvoice deep-copies an invoice.
func cloneInvoice(inv *Invoice) *Invoice {
	if inv == nil {
		return nil
	}
	out := *inv
	out.Lines = append([]InvoiceLine(nil), inv.Lines...)
	return &out
}

// MemStore is an in-memory Store (deep copies in and out). It models a
// durable store: data survives engine restarts within the process.
type MemStore struct {
	mu       sync.Mutex
	subs     map[string]*Subscription
	invoices map[string]*Invoice
}

func NewMemStore() *MemStore {
	return &MemStore{
		subs:     make(map[string]*Subscription),
		invoices: make(map[string]*Invoice),
	}
}

func (m *MemStore) SaveSubscription(s *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[s.ID] = cloneSubscription(s)
	return nil
}

func (m *MemStore) SaveInvoice(inv *Invoice) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invoices[inv.ID] = cloneInvoice(inv)
	return nil
}

func (m *MemStore) Load() ([]*Subscription, []*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subs := make([]*Subscription, 0, len(m.subs))
	for _, s := range m.subs {
		subs = append(subs, cloneSubscription(s))
	}
	invs := make([]*Invoice, 0, len(m.invoices))
	for _, inv := range m.invoices {
		invs = append(invs, cloneInvoice(inv))
	}
	return subs, invs, nil
}
