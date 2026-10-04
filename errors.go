package subscription

import "errors"

var (
	ErrNotFound             = errors.New("subscription: not found")
	ErrAlreadyExists        = errors.New("subscription: already exists")
	ErrPlanNotFound         = errors.New("subscription: plan not found")
	ErrInvalidState         = errors.New("subscription: invalid state for operation")
	ErrNoPendingChange      = errors.New("subscription: no pending change to cancel")
	ErrDuplicateRequest     = errors.New("subscription: duplicate request")
	ErrSubscriptionEnded    = errors.New("subscription: already ended")
	ErrChangeNotCancellable = errors.New("subscription: change no longer cancellable")
)
