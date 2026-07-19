package ratelimit

import "errors"

var (
	ErrUnknownAPIKey            = errors.New("unknown api key")
	ErrReservationNotFound      = errors.New("reservation not found")
	ErrReservationExpired       = errors.New("reservation expired")
	ErrAlreadySettled           = errors.New("reservation already settled")
	ErrAmountExceedsReservation = errors.New("amount exceeds reservation")
)
