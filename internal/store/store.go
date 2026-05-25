// Package store provides Supabase Postgres access via pgx pools (tx + session).
package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SubcentsPerCent is the conversion factor between internal "subcent" units and user-facing cents.
// 1 subcent = 1/100,000 of a cent. This matches the precision of tier_pricing.usd_cents_per_second_x100.
const SubcentsPerCent int64 = 100_000

// Store holds connection pools and DB helpers.
type Store struct {
	Tx      *pgxpool.Pool
	Session *pgxpool.Pool
}

// Close releases both pools.
func (s *Store) Close() {
	if s == nil {
		return
	}
	if s.Tx != nil {
		s.Tx.Close()
	}
	if s.Session != nil {
		s.Session.Close()
	}
}

// PingTx checks primary pool connectivity.
func (s *Store) PingTx(ctx context.Context) error {
	return s.Tx.Ping(ctx)
}

// Agent row (lightning path principal).
type Agent struct {
	ID           uuid.UUID
	JWTSub       *uuid.UUID // nil for MCP anonymous-L402 agents until linked to a dashboard user
	BalanceMsats int64
	WarningLevel string
}

// User row (dashboard / credits).
type User struct {
	ID                    uuid.UUID
	Email                 string
	CreditBalanceSubcents int64
	WarningLevel          string
}

// APIKey row.
type APIKey struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	KeyHash   string
	KeyPrefix string
	Name      string
	IsActive  bool
}

// Sandbox row.
type Sandbox struct {
	ID              uuid.UUID
	AgentID         *uuid.UUID
	UserID          *uuid.UUID
	APIKeyID        *uuid.UUID
	BillingMethod   string
	Tier            string
	Template        string
	Status          string
	Runtime         string
	PodID           *string
	PodProxyURL     *string
	NetworkVolumeID *string
	ConsumedMsats   int64

	// Per-sandbox lightning prepay (sandbox-token path).
	SandboxToken              *string
	PrepaidBalanceMsats       int64
	PendingInvoiceBolt11      *string
	PendingInvoicePaymentHash []byte
	PendingInvoiceAmountMsats *int64
	PendingInvoiceExpiresAt   *time.Time

	// Per-sandbox credit pool (credits billing).
	PrepaidBalanceSubcents int64
}

// TierPricing row.
type TierPricing struct {
	Tier                  string
	MsatsPerSecond        int64
	USDCentsPerSecondX100 int64
}

// BillingInterval for bulk insert (lightning).
type BillingInterval struct {
	SandboxID uuid.UUID
	AgentID   uuid.UUID
	Start     interface{} // time
	End       interface{}
	Msats     int64
}

// CreditCharge for batched credit path metering.
type CreditCharge struct {
	UserID         uuid.UUID
	SandboxID      uuid.UUID
	AmountSubcents int64
	Start          interface{}
	End            interface{}
	Tier           string
	Description    string
}

// CreditTxSummary is a dashboard-friendly ledger row.
type CreditTxSummary struct {
	ID           uuid.UUID
	Type         string
	Amount       int64
	BalanceAfter int64
	Description  string
	CreatedAt    time.Time
}
