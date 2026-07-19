// Package ratelimit implements an API quota ledger on top of TigerBeetle.
//
// Each API key is a TigerBeetle account with DebitsMustNotExceedCredits set,
// so its available balance (credits granted minus debits used minus debits
// pending) can never go negative under any amount of concurrency -
// TigerBeetle processes transfers against a single account strictly one at a
// time, which is what rules out double-spend without any locking on our
// side. Every request reserves capacity with a pending (two-phase) transfer;
// callers then post (commit) or void (release) it once they know whether the
// downstream work succeeded.
package ratelimit

import (
	"fmt"
	"math"
	"time"

	tb "github.com/tigerbeetle/tigerbeetle-go"
)

const (
	ledgerID = 1

	issuerCode = 1
	sinkCode   = 2
	apiKeyCode = 3

	grantCode = 1
	usageCode = 2
)

var (
	// issuerAccountID is the source account that quota grants are debited
	// from. It carries no balance constraints, so it can fund any number
	// of API key accounts.
	issuerAccountID = tb.ToUint128(1)

	// sinkAccountID is the destination for every usage transfer. Its
	// posted balance is effectively a running total of settled request
	// cost across all keys; it isn't otherwise used by this service.
	sinkAccountID = tb.ToUint128(2)
)

type Ledger struct {
	client                    tb.Client
	defaultReservationTimeout uint32 // seconds
}

// New wraps a TigerBeetle client with quota-ledger operations. defaultReservationTimeout
// is how long a reservation may stay pending before TigerBeetle auto-voids it.
func New(client tb.Client, defaultReservationTimeout time.Duration) *Ledger {
	return &Ledger{
		client:                    client,
		defaultReservationTimeout: uint32(defaultReservationTimeout.Seconds()),
	}
}

// Bootstrap idempotently creates the system accounts used internally. Safe
// to call on every startup.
func (l *Ledger) Bootstrap() error {
	results, err := l.client.CreateAccounts([]tb.Account{
		{ID: issuerAccountID, Ledger: ledgerID, Code: issuerCode},
		{ID: sinkAccountID, Ledger: ledgerID, Code: sinkCode},
	})
	if err != nil {
		return fmt.Errorf("bootstrap accounts: %w", err)
	}
	for i, r := range results {
		if !accountOK(r.Status) {
			return fmt.Errorf("bootstrap account %d: %s", i, r.Status)
		}
	}
	return nil
}

func (l *Ledger) ensureKeyAccount(id tb.Uint128) error {
	results, err := l.client.CreateAccounts([]tb.Account{
		{
			ID:     id,
			Ledger: ledgerID,
			Code:   apiKeyCode,
			Flags:  tb.AccountFlags{DebitsMustNotExceedCredits: true}.ToUint16(),
		},
	})
	if err != nil {
		return fmt.Errorf("create key account: %w", err)
	}
	if !accountOK(results[0].Status) {
		return fmt.Errorf("create key account: %s", results[0].Status)
	}
	return nil
}

func accountOK(status tb.CreateAccountStatus) bool {
	return status == tb.AccountCreated || status == tb.AccountExists
}

// Grant tops up an API key's quota by amount, creating the key's account
// first if this is the first time it's been seen. Grants are additive, so
// calling it again adds more capacity rather than replacing it.
func (l *Ledger) Grant(apiKey string, amount uint64) (tb.Uint128, error) {
	id := DeriveAccountID(apiKey)
	if err := l.ensureKeyAccount(id); err != nil {
		return tb.Uint128{}, err
	}

	results, err := l.client.CreateTransfers([]tb.Transfer{
		{
			ID:              tb.ID(),
			DebitAccountID:  issuerAccountID,
			CreditAccountID: id,
			Amount:          tb.ToUint128(amount),
			Ledger:          ledgerID,
			Code:            grantCode,
		},
	})
	if err != nil {
		return tb.Uint128{}, fmt.Errorf("grant transfer: %w", err)
	}
	status := results[0].Status
	if status != tb.TransferCreated && status != tb.TransferExists {
		return tb.Uint128{}, fmt.Errorf("grant transfer: %s", status)
	}
	return id, nil
}

type ReserveResult struct {
	Allowed        bool
	ReservationID  tb.Uint128
	TimeoutSeconds uint32
}

// Reserve atomically checks and decrements cost units of quota for apiKey,
// using this Ledger's default reservation timeout. It does not finalize the
// spend - call Commit or Void with the returned ReservationID once the
// downstream request has run.
func (l *Ledger) Reserve(apiKey string, cost uint64) (ReserveResult, error) {
	return l.ReserveWithTimeout(apiKey, cost, l.defaultReservationTimeout)
}

func (l *Ledger) ReserveWithTimeout(apiKey string, cost uint64, timeoutSeconds uint32) (ReserveResult, error) {
	id := DeriveAccountID(apiKey)
	reservationID := tb.ID()

	results, err := l.client.CreateTransfers([]tb.Transfer{
		{
			ID:              reservationID,
			DebitAccountID:  id,
			CreditAccountID: sinkAccountID,
			Amount:          tb.ToUint128(cost),
			Ledger:          ledgerID,
			Code:            usageCode,
			Timeout:         timeoutSeconds,
			Flags:           tb.TransferFlags{Pending: true}.ToUint16(),
		},
	})
	if err != nil {
		return ReserveResult{}, fmt.Errorf("reserve transfer: %w", err)
	}

	switch results[0].Status {
	case tb.TransferCreated:
		return ReserveResult{Allowed: true, ReservationID: reservationID, TimeoutSeconds: timeoutSeconds}, nil
	case tb.TransferExceedsCredits:
		return ReserveResult{Allowed: false}, nil
	case tb.TransferDebitAccountNotFound:
		return ReserveResult{}, ErrUnknownAPIKey
	default:
		return ReserveResult{}, fmt.Errorf("reserve transfer: %s", results[0].Status)
	}
}

// Commit finalizes a reservation, permanently spending its quota. If amount
// is nil the full reserved amount is posted; otherwise it must be <= the
// amount originally reserved, and any unused remainder is released back to
// the account.
func (l *Ledger) Commit(reservationID tb.Uint128, amount *uint64) error {
	amt := tb.AmountMax
	if amount != nil {
		amt = tb.ToUint128(*amount)
	}

	results, err := l.client.CreateTransfers([]tb.Transfer{
		{
			ID:        tb.ID(),
			PendingID: reservationID,
			Amount:    amt,
			Ledger:    ledgerID,
			Code:      usageCode,
			Flags:     tb.TransferFlags{PostPendingTransfer: true}.ToUint16(),
		},
	})
	if err != nil {
		return fmt.Errorf("commit transfer: %w", err)
	}
	return classifySettleStatus(results[0].Status)
}

// Void releases a reservation without spending anything, restoring its
// quota to the account. Use this when the downstream work the quota was
// reserved for failed.
func (l *Ledger) Void(reservationID tb.Uint128) error {
	results, err := l.client.CreateTransfers([]tb.Transfer{
		{
			ID:        tb.ID(),
			PendingID: reservationID,
			Ledger:    ledgerID,
			Code:      usageCode,
			Flags:     tb.TransferFlags{VoidPendingTransfer: true}.ToUint16(),
		},
	})
	if err != nil {
		return fmt.Errorf("void transfer: %w", err)
	}
	return classifySettleStatus(results[0].Status)
}

func classifySettleStatus(status tb.CreateTransferStatus) error {
	switch status {
	case tb.TransferCreated, tb.TransferExists:
		return nil
	case tb.TransferPendingTransferNotFound:
		return ErrReservationNotFound
	case tb.TransferPendingTransferExpired:
		return ErrReservationExpired
	case tb.TransferPendingTransferAlreadyPosted, tb.TransferPendingTransferAlreadyVoided:
		return ErrAlreadySettled
	case tb.TransferExceedsPendingTransferAmount:
		return ErrAmountExceedsReservation
	default:
		return fmt.Errorf("settle transfer: %s", status)
	}
}

type Balance struct {
	Granted   uint64
	Used      uint64
	Pending   uint64
	Available uint64
}

// GetBalance returns the current quota state for an API key.
func (l *Ledger) GetBalance(apiKey string) (Balance, error) {
	id := DeriveAccountID(apiKey)
	accounts, err := l.client.LookupAccounts([]tb.Uint128{id})
	if err != nil {
		return Balance{}, fmt.Errorf("lookup account: %w", err)
	}
	if len(accounts) == 0 {
		return Balance{}, ErrUnknownAPIKey
	}

	a := accounts[0]
	granted := toUint64(a.CreditsPosted)
	used := toUint64(a.DebitsPosted)
	pending := toUint64(a.DebitsPending)

	var available uint64
	if granted > used+pending {
		available = granted - used - pending
	}

	return Balance{Granted: granted, Used: used, Pending: pending, Available: available}, nil
}

func toUint64(v tb.Uint128) uint64 {
	lo, hi := v.Uint64()
	if hi != 0 {
		return math.MaxUint64
	}
	return lo
}
