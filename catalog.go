package subscription

import (
	"sort"
	"sync"
	"time"
)

// Catalog holds plans and their versioned prices.
type Catalog struct {
	mu    sync.RWMutex
	plans map[string]*planEntry
}

type planEntry struct {
	plan   Plan
	prices []PriceVersion // sorted by EffectiveFrom ascending
}

func NewCatalog() *Catalog {
	return &Catalog{plans: make(map[string]*planEntry)}
}

// UpsertPlan adds or replaces a plan definition. Existing price versions
// for the plan are preserved.
func (c *Catalog) UpsertPlan(p Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.plans[p.ID]
	if !ok {
		e = &planEntry{}
		c.plans[p.ID] = e
	}
	e.plan = p
}

// SetPrice registers a price version for a plan. A version with the same
// EffectiveFrom replaces the old one; versions take effect at their
// EffectiveFrom time and never retroactively.
func (c *Catalog) SetPrice(planID string, pv PriceVersion) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.plans[planID]
	if !ok {
		return ErrPlanNotFound
	}
	for i, existing := range e.prices {
		if existing.EffectiveFrom.Equal(pv.EffectiveFrom) {
			e.prices[i] = pv
			return nil
		}
	}
	e.prices = append(e.prices, pv)
	sort.Slice(e.prices, func(i, j int) bool {
		return e.prices[i].EffectiveFrom.Before(e.prices[j].EffectiveFrom)
	})
	return nil
}

// PriceAt returns the price version effective at time t: the latest version
// whose EffectiveFrom is <= t. Already-issued invoices are unaffected by
// later versions because they snapshot the price at issuance.
func (c *Catalog) PriceAt(planID string, t time.Time) (PriceVersion, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.plans[planID]
	if !ok {
		return PriceVersion{}, ErrPlanNotFound
	}
	var best *PriceVersion
	for i := range e.prices {
		if !e.prices[i].EffectiveFrom.After(t) {
			best = &e.prices[i]
		} else {
			break
		}
	}
	if best == nil {
		return PriceVersion{}, ErrPlanNotFound
	}
	return *best, nil
}

// Plan returns the plan definition.
func (c *Catalog) Plan(id string) (Plan, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.plans[id]
	if !ok {
		return Plan{}, false
	}
	return e.plan, true
}
