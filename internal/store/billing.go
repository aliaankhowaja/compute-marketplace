package store

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) InsertBillingIntervals(ctx context.Context, agentID uuid.UUID, rows []BillingInterval) error {
	for _, r := range rows {
		if r.Msats <= 0 {
			continue
		}
		_, err := s.Tx.Exec(ctx, `INSERT INTO public.billing_intervals (sandbox_id, agent_id, interval_start, interval_end, msats_consumed)
			VALUES ($1,$2,$3,$4,$5)`,
			r.SandboxID, agentID, r.Start, r.End, r.Msats)
		if err != nil {
			return err
		}
		if _, err := s.Tx.Exec(ctx, `UPDATE public.agents SET balance_msats = balance_msats - $2 WHERE id = $1 AND balance_msats - $2 >= -1000000`,
			agentID, r.Msats); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListBillingIntervalsByAgent(ctx context.Context, agentID uuid.UUID, limit int) ([]BillingInterval, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.Tx.Query(ctx, `SELECT sandbox_id, agent_id, interval_start, interval_end, msats_consumed
		FROM public.billing_intervals WHERE agent_id = $1 ORDER BY created_at DESC LIMIT $2`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BillingInterval
	for rows.Next() {
		var r BillingInterval
		var aid uuid.UUID
		if err := rows.Scan(&r.SandboxID, &aid, &r.Start, &r.End, &r.Msats); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TryFinalizeTopUp inserts a settled top-up row; duplicate idempotency_key is ignored.
func (s *Store) TryFinalizeTopUp(ctx context.Context, agentID uuid.UUID, idem string, amountMsats int64, payHash []byte, preimage []byte, inv string) error {
	_, err := s.Tx.Exec(ctx, `INSERT INTO public.top_up_intervals (agent_id, idempotency_key, amount_msats, payment_hash, preimage, bolt12_invoice, status, settled_at)
		VALUES ($1,$2,$3,$4,$5,$6,'settled', now())
		ON CONFLICT (idempotency_key) DO NOTHING`,
		agentID, idem, amountMsats, payHash, preimage, inv)
	return err
}

func (s *Store) InsertAuditLog(ctx context.Context, eventType string, userID, agentID, sandboxID, apiKeyID *uuid.UUID, authMethod *string, payload interface{}) error {
	_, err := s.Tx.Exec(ctx, `INSERT INTO public.audit_log (event_type, user_id, agent_id, sandbox_id, api_key_id, auth_method, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		eventType, userID, agentID, sandboxID, apiKeyID, authMethod, payload)
	return err
}

func (s *Store) InsertPendingDeposit(ctx context.Context, agentID uuid.UUID, payHash []byte, bolt11 string, amountMsats int64) error {
	_, err := s.Tx.Exec(ctx, `INSERT INTO public.pending_deposits (agent_id, payment_hash, bolt11_invoice, amount_msats, status) VALUES ($1,$2,$3,$4,'pending')`,
		agentID, payHash, bolt11, amountMsats)
	return err
}

// DepositWireStatus is returned by GET /wallet/deposit status.
type DepositWireStatus struct {
	Status        string  // pending | settled | expired | failed
	AmountMsats   int64
	SettledAt     *time.Time
	Bolt11Invoice *string // set while row is pending when still in pending_deposits
}

// GetDepositStatusForAgent resolves a pending_deposits row by payment hash or settled top_up_intervals history.
func (s *Store) GetDepositStatusForAgent(ctx context.Context, agentID uuid.UUID, payHash []byte) (*DepositWireStatus, error) {
	if len(payHash) != 32 {
		return nil, nil
	}
	var row DepositWireStatus
	var bolt string
	var settled *time.Time

	err := s.Tx.QueryRow(ctx, `
		SELECT status, amount_msats, settled_at, bolt11_invoice
		FROM public.pending_deposits WHERE agent_id = $1 AND payment_hash = $2`,
		agentID, payHash).Scan(&row.Status, &row.AmountMsats, &settled, &bolt)

	if errors.Is(err, pgx.ErrNoRows) {
		var st2 DepositWireStatus
		err = s.Tx.QueryRow(ctx, `
			SELECT amount_msats, settled_at
			FROM public.top_up_intervals
			WHERE agent_id = $1 AND payment_hash = $2 AND status = 'settled'
			ORDER BY settled_at DESC NULLS LAST LIMIT 1`,
			agentID, payHash).Scan(&st2.AmountMsats, &settled)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		st2.Status = "settled"
		st2.SettledAt = settled
		return &st2, nil
	}
	if err != nil {
		return nil, err
	}
	row.SettledAt = settled
	if strings.TrimSpace(bolt) != "" && row.Status == "pending" {
		b := bolt
		row.Bolt11Invoice = &b
	}
	return &row, nil
}

// PendingDepositRow is a pending manual deposit.
type PendingDepositRow struct {
	ID        uuid.UUID
	AgentID   uuid.UUID
	PayHash   []byte
	Invoice   string
	Amount    int64
	CreatedAt time.Time
}

func (s *Store) ListPendingDepositRows(ctx context.Context, limit int) ([]PendingDepositRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.Tx.Query(ctx, `SELECT id, agent_id, payment_hash, bolt11_invoice, amount_msats, created_at FROM public.pending_deposits WHERE status = 'pending' ORDER BY created_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingDepositRow
	for rows.Next() {
		var r PendingDepositRow
		if err := rows.Scan(&r.ID, &r.AgentID, &r.PayHash, &r.Invoice, &r.Amount, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MarkDepositSettled(ctx context.Context, id uuid.UUID) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.pending_deposits SET status = 'settled', settled_at = now() WHERE id = $1`, id)
	return err
}

// SettlePendingDeposit credits the agent, records top_up_intervals, and marks the deposit settled (single transaction).
// If the top-up row already exists (idempotent replay), only ensures pending row is marked settled.
func (s *Store) SettlePendingDeposit(ctx context.Context, dep PendingDepositRow, preimage []byte, bolt11 string) error {
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	idem := "deposit:" + hex.EncodeToString(dep.PayHash)
	ct, err := tx.Exec(ctx, `INSERT INTO public.top_up_intervals (agent_id, idempotency_key, amount_msats, payment_hash, preimage, bolt12_invoice, status, settled_at)
		VALUES ($1,$2,$3,$4,$5,$6,'settled', now())
		ON CONFLICT (idempotency_key) DO NOTHING`,
		dep.AgentID, idem, dep.Amount, dep.PayHash, preimage, bolt11)
	if err != nil {
		return err
	}
	if ct.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `UPDATE public.agents SET balance_msats = balance_msats + $2 WHERE id = $1`, dep.AgentID, dep.Amount); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE public.pending_deposits SET status = 'settled', settled_at = now() WHERE id = $1 AND status = 'pending'`, dep.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListCreditTransactionsByUser(ctx context.Context, userID uuid.UUID, limit int) ([]CreditTxSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.Tx.Query(ctx, `SELECT id, type, amount_usd_cents, balance_after_usd_cents, description, created_at FROM public.credit_transactions WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditTxSummary
	for rows.Next() {
		var r CreditTxSummary
		if err := rows.Scan(&r.ID, &r.Type, &r.Amount, &r.BalanceAfter, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
