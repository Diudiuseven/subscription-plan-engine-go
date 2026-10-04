package subscription

import "time"

// prorationResult is the outcome of switching plans mid-period.
type prorationResult struct {
	UsedOldCents int64 // value consumed on the old plan up to the switch
	UnusedCents  int64 // remaining value of the old plan (refundable/creditable)
	DueCents     int64 // cost of the new plan for the rest of the period
	DeltaCents   int64 // DueCents - UnusedCents; >0 charge now, <0 carry as credit
}

// prorate computes per-second proration between two prices over a period.
//
// Reconciliation guarantee: UsedOldCents + UnusedCents == oldPrice.AmountCents
// exactly, so a mixed period billed as (used old + remaining new) always
// matches a line-by-line recomputation at the full-cycle rate of each plan.
func prorate(oldPrice, newPrice PriceVersion, periodStart, periodEnd, at time.Time) prorationResult {
	total := periodEnd.Sub(periodStart)
	if total <= 0 {
		return prorationResult{}
	}
	if at.Before(periodStart) {
		at = periodStart
	}
	if at.After(periodEnd) {
		at = periodEnd
	}
	elapsed := at.Sub(periodStart)
	remaining := periodEnd.Sub(at)

	usedOld := prorataAmount(oldPrice.AmountCents, elapsed, total)
	unused := oldPrice.AmountCents - usedOld // exact complement, no rounding drift
	due := prorataAmount(newPrice.AmountCents, remaining, total)

	return prorationResult{
		UsedOldCents: usedOld,
		UnusedCents:  unused,
		DueCents:     due,
		DeltaCents:   due - unused,
	}
}

// prorataAmount computes round-half-up(amount * part / whole) in integer cents.
// Durations are truncated to seconds to keep the multiplication within int64.
func prorataAmount(amountCents int64, part, whole time.Duration) int64 {
	if whole <= 0 || part <= 0 {
		return 0
	}
	p := int64(part / time.Second)
	w := int64(whole / time.Second)
	if w <= 0 {
		return 0
	}
	return (amountCents*p + w/2) / w
}
