package subscription

import "time"

// Status is the lifecycle state of a subscription.
type Status string

const (
	StatusTrialing  Status = "trialing"
	StatusActive    Status = "active"
	StatusPastDue   Status = "past_due"
	StatusSuspended Status = "suspended"
	StatusEnded     Status = "ended"
)

// ChangeKind distinguishes upgrade from downgrade.
type ChangeKind string

const (
	ChangeUpgrade   ChangeKind = "upgrade"
	ChangeDowngrade ChangeKind = "downgrade"
)

// InvoiceStatus is the payment state of an invoice.
type InvoiceStatus string

const (
	InvoiceOpen   InvoiceStatus = "open"
	InvoicePaid   InvoiceStatus = "paid"
	InvoiceVoid   InvoiceStatus = "void"
	InvoiceFailed InvoiceStatus = "failed"
)

// LineKind classifies invoice lines.
type LineKind string

const (
	LineSubscription LineKind = "subscription"
	LineProration    LineKind = "proration"
	LineCredit       LineKind = "credit"
)

// ExhaustedAction is what happens when all payment retries fail.
type ExhaustedAction string

const (
	ActionSuspend   ExhaustedAction = "suspend"
	ActionTerminate ExhaustedAction = "terminate"
)

// Plan describes a subscription plan. Prices are versioned separately.
type Plan struct {
	ID        string
	Name      string
	Rank      int // higher rank = higher tier, used to classify upgrade/downgrade
	TrialDays int // trial length granted when subscribing to this plan
}

// PriceVersion is a plan price effective from a point in time.
// Historical invoices keep the price that was effective when they were issued.
type PriceVersion struct {
	ID            string
	AmountCents   int64
	IntervalDays  int
	EffectiveFrom time.Time
}

// RetryPolicy describes how failed charges are retried.
type RetryPolicy struct {
	Intervals []time.Duration // delay before each successive retry
}

// Config holds engine-level tunables.
type Config struct {
	GracePeriod      time.Duration   // entitlement continues during grace after first failure
	Retry            RetryPolicy     // retry schedule for failed charges
	OnRetryExhausted ExhaustedAction // suspend or terminate after retries+grace
}

// ScheduledChange is a plan change that takes effect at a future time.
type ScheduledChange struct {
	ID           string
	TargetPlanID string
	Kind         ChangeKind
	IssuedAt     time.Time
	EffectiveAt  time.Time
}

// Subscription is the mutable state of one account's subscription.
type Subscription struct {
	ID                 string
	AccountID          string
	PlanID             string
	Status             Status
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	TrialEnd           *time.Time
	CancelAtPeriodEnd  bool
	PendingChange      *ScheduledChange
	CreditCents        int64 // carry-over credit applied to future invoices
	Version            int64 // bumped on every mutation; snapshots are consistent
}

// InvoiceLine is a single charge/credit line on an invoice.
type InvoiceLine struct {
	Kind        LineKind
	Description string
	AmountCents int64 // positive = charge, negative = credit
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// Invoice is an immutable-ish billing record; only payment state mutates.
type Invoice struct {
	ID             string
	SubscriptionID string
	AccountID      string
	Lines          []InvoiceLine
	AmountCents    int64 // sum of charge lines
	CreditApplied  int64 // credit balance consumed
	NetCents       int64 // AmountCents - CreditApplied
	Status         InvoiceStatus
	IssuedAt       time.Time
	Attempts       int
	NextAttemptAt  time.Time
	GraceUntil     time.Time
}
