package store

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
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

// ApplyAutonomousTopUp records a settled pull top-up and credits the agent balance once per idempotency_key.
func (s *Store) ApplyAutonomousTopUp(ctx context.Context, agentID uuid.UUID, idem string, amountMsats int64, payHash []byte, preimage []byte, inv string) error {
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx, `INSERT INTO public.top_up_intervals (agent_id, idempotency_key, amount_msats, payment_hash, preimage, bolt12_invoice, status, settled_at)
		VALUES ($1,$2,$3,$4,$5,$6,'settled', now())
		ON CONFLICT (idempotency_key) DO NOTHING`,
		agentID, idem, amountMsats, payHash, preimage, inv)
	if err != nil {
		return err
	}
	if ct.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `UPDATE public.agents SET balance_msats = balance_msats + $2 WHERE id = $1`, agentID, amountMsats); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
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
