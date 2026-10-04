package engine

import "errors"

var (
	ErrNotFound            = errors.New("subscription not found")
	ErrPlanNotFound        = errors.New("plan not found")
	ErrInvalidState        = errors.New("operation not allowed in current state")
	ErrNoPendingChange     = errors.New("no pending change to cancel")
	ErrDuplicateRequest    = errors.New("duplicate request")
	ErrChangeAlreadyExists = errors.New("a pending change already exists")
)
