package ratelimit

import "errors"

var (
	ErrUnknownAPIKey            = errors.New("unknown api key")
	ErrKeyConfigMismatch        = errors.New("key already exists with a different capacity or refill_per_interval")
	ErrReservationNotFound      = errors.New("reservation not found")
	ErrReservationExpired       = errors.New("reservation expired")
	ErrAlreadySettled           = errors.New("reservation already settled")
	ErrAmountExceedsReservation = errors.New("amount exceeds reservation")
)
