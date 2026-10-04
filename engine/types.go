// Package engine 实现订阅生命周期与套餐变更引擎。
package engine

import "time"

// Status 订阅状态。
type Status string

const (
	StatusTrialing  Status = "trialing"
	StatusActive    Status = "active"
	StatusPastDue   Status = "past_due"  // 扣费失败，处于宽限期
	StatusSuspended Status = "suspended" // 重试耗尽后挂起
	StatusCanceled  Status = "canceled"  // 已取消，等待周期结束
	StatusExpired   Status = "expired"   // 已终止
)

// ChangeKind 变更类型。
type ChangeKind string

const (
	ChangeUpgrade   ChangeKind = "upgrade"
	ChangeDowngrade ChangeKind = "downgrade"
)

// ExhaustedPolicy 重试耗尽后的策略。
type ExhaustedPolicy string

const (
	PolicyTerminate ExhaustedPolicy = "terminate"
	PolicySuspend   ExhaustedPolicy = "suspend"
)

// Plan 套餐定义。
type Plan struct {
	ID           string
	Name         string
	PriceCents   int64 // 每周期价格（分）
	IntervalDays int   // 计费周期天数
	TrialDays    int   // 试用天数，0 表示无试用
	Entitlements []string
}

// Config 引擎配置。
type Config struct {
	GracePeriodDays   int
	MaxRetries        int
	RetryIntervalDays int
	OnExhausted       ExhaustedPolicy
}

// PendingChange 未来生效的套餐变更。
type PendingChange struct {
	ID          string
	Kind        ChangeKind
	ToPlanID    string
	EffectiveAt time.Time
	CreatedAt   time.Time
}

// LedgerEntry 账户账本条目（分），正数为应扣，负数为结余留抵。
type LedgerEntry struct {
	ID        string
	Kind      string // charge / proration_credit / proration_debit / credit_applied
	Amount    int64
	Reason    string
	CreatedAt time.Time
}

// Invoice 账单。
type Invoice struct {
	ID             string
	SubscriptionID string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	AmountCents    int64 // 实际应收（已抵扣结余）
	Paid           bool
	CreatedAt      time.Time
}

// Subscription 订阅聚合（引擎内部持有，外部读取为快照拷贝）。
type Subscription struct {
	ID                 string
	AccountID          string
	PlanID             string
	Status             Status
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	TrialEnd           time.Time
	CreditCents        int64 // 可用结余（留抵）
	PendingChange      *PendingChange
	CancelAtPeriodEnd  bool
	FailedAttempts     int
	NextRetryAt        time.Time
	GraceUntil         time.Time
	OutstandingCents   int64 // 欠费金额
	Version            int64 // 每次状态迁移递增，用于并发与幂等
}
