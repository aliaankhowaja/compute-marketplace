package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const sandboxColumns = `id, agent_id, user_id, api_key_id, billing_method, tier, template, status, runtime,
		pod_id, pod_proxy_url, network_volume_id, COALESCE(consumed_msats, 0),
		sandbox_token, COALESCE(prepaid_balance_msats, 0),
		pending_invoice_bolt11, pending_invoice_payment_hash, pending_invoice_amount_msats, pending_invoice_expires_at`

func scanSandbox(row pgx.Row, sb *Sandbox) error {
	return row.Scan(
		&sb.ID, &sb.AgentID, &sb.UserID, &sb.APIKeyID, &sb.BillingMethod, &sb.Tier, &sb.Template, &sb.Status, &sb.Runtime,
		&sb.PodID, &sb.PodProxyURL, &sb.NetworkVolumeID, &sb.ConsumedMsats,
		&sb.SandboxToken, &sb.PrepaidBalanceMsats,
		&sb.PendingInvoiceBolt11, &sb.PendingInvoicePaymentHash, &sb.PendingInvoiceAmountMsats, &sb.PendingInvoiceExpiresAt,
	)
}

func (s *Store) InsertSandbox(ctx context.Context, sb *Sandbox) error {
	if sb == nil {
		return fmt.Errorf("nil sandbox")
	}
	if sb.ID == uuid.Nil {
		sb.ID = uuid.New()
	}
	q := `INSERT INTO public.sandboxes (
		id, agent_id, user_id, api_key_id, billing_method, tier, template, status, runtime,
		pod_id, pod_proxy_url, network_volume_id, consumed_msats,
		sandbox_token, prepaid_balance_msats,
		pending_invoice_bolt11, pending_invoice_payment_hash, pending_invoice_amount_msats, pending_invoice_expires_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`
	_, err := s.Tx.Exec(ctx, q,
		sb.ID, sb.AgentID, sb.UserID, sb.APIKeyID, sb.BillingMethod, sb.Tier, sb.Template, sb.Status, sb.Runtime,
		sb.PodID, sb.PodProxyURL, sb.NetworkVolumeID, sb.ConsumedMsats,
		sb.SandboxToken, sb.PrepaidBalanceMsats,
		sb.PendingInvoiceBolt11, sb.PendingInvoicePaymentHash, sb.PendingInvoiceAmountMsats, sb.PendingInvoiceExpiresAt,
	)
	return err
}

func (s *Store) GetSandbox(ctx context.Context, id uuid.UUID) (*Sandbox, error) {
	q := `SELECT ` + sandboxColumns + ` FROM public.sandboxes WHERE id = $1`
	var sb Sandbox
	if err := scanSandbox(s.Tx.QueryRow(ctx, q, id), &sb); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &sb, nil
}

// GetSandboxByToken resolves a sandbox by its opaque sandbox_token bearer.
func (s *Store) GetSandboxByToken(ctx context.Context, token string) (*Sandbox, error) {
	if token == "" {
		return nil, nil
	}
	q := `SELECT ` + sandboxColumns + ` FROM public.sandboxes WHERE sandbox_token = $1`
	var sb Sandbox
	if err := scanSandbox(s.Tx.QueryRow(ctx, q, token), &sb); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &sb, nil
}

// LookupSandboxByToken implements auth.SandboxTokenLookup.
func (s *Store) LookupSandboxByToken(ctx context.Context, token string) (uuid.UUID, bool, error) {
	sb, err := s.GetSandboxByToken(ctx, token)
	if err != nil {
		return uuid.UUID{}, false, err
	}
	if sb == nil {
		return uuid.UUID{}, false, nil
	}
	return sb.ID, true, nil
}

func (s *Store) UpdateSandboxStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.sandboxes SET status = $2 WHERE id = $1`, id, status)
	return err
}

func (s *Store) UpdateSandboxPod(ctx context.Context, id uuid.UUID, podID, proxyURL, volumeID *string) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.sandboxes SET pod_id = $2, pod_proxy_url = $3, network_volume_id = $4 WHERE id = $1`,
		id, podID, proxyURL, volumeID)
	return err
}

func (s *Store) SetSandboxConsumedMsats(ctx context.Context, id uuid.UUID, consumed int64) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.sandboxes SET consumed_msats = $2 WHERE id = $1`, id, consumed)
	return err
}

func (s *Store) TerminateSandbox(ctx context.Context, id uuid.UUID) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.sandboxes SET status = 'terminated', terminated_at = now() WHERE id = $1`, id)
	return err
}

// SetSandboxPendingInvoice stores the BOLT11 the LLM must pay to extend the sandbox runtime.
func (s *Store) SetSandboxPendingInvoice(ctx context.Context, id uuid.UUID, bolt11 string, payHash []byte, amountMsats int64, expiresAt time.Time) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.sandboxes
		SET pending_invoice_bolt11 = $2,
		    pending_invoice_payment_hash = $3,
		    pending_invoice_amount_msats = $4,
		    pending_invoice_expires_at = $5
		WHERE id = $1`,
		id, bolt11, payHash, amountMsats, expiresAt)
	return err
}

// ClearSandboxPendingInvoice clears the pending-invoice fields after settlement or expiry.
func (s *Store) ClearSandboxPendingInvoice(ctx context.Context, id uuid.UUID) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.sandboxes
		SET pending_invoice_bolt11 = NULL,
		    pending_invoice_payment_hash = NULL,
		    pending_invoice_amount_msats = NULL,
		    pending_invoice_expires_at = NULL
		WHERE id = $1`, id)
	return err
}

// IncrementSandboxPrepaidMsats credits the per-sandbox prepaid balance and returns the new total.
func (s *Store) IncrementSandboxPrepaidMsats(ctx context.Context, id uuid.UUID, delta int64) (int64, error) {
	var bal int64
	err := s.Tx.QueryRow(ctx,
		`UPDATE public.sandboxes SET prepaid_balance_msats = prepaid_balance_msats + $2 WHERE id = $1 RETURNING prepaid_balance_msats`,
		id, delta).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("sandbox not found")
	}
	return bal, err
}

func (s *Store) CountRunningSandboxesByUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	var n int64
	err := s.Tx.QueryRow(ctx, `SELECT count(*) FROM public.sandboxes WHERE user_id = $1 AND status IN ('pending','running')`, userID).Scan(&n)
	return n, err
}

func (s *Store) CountRunningSandboxesByAgent(ctx context.Context, agentID uuid.UUID) (int64, error) {
	var n int64
	err := s.Tx.QueryRow(ctx, `SELECT count(*) FROM public.sandboxes WHERE agent_id = $1 AND status IN ('pending','running')`, agentID).Scan(&n)
	return n, err
}

// MaxMsatsPerSecondRunningSandboxesForAgent returns peak msats/sec among active sandboxes (pending/running) for metering hints.
func (s *Store) MaxMsatsPerSecondRunningSandboxesForAgent(ctx context.Context, agentID uuid.UUID) (int64, error) {
	var max int64
	err := s.Tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(tp.msats_per_second), 0)
		FROM public.sandboxes s
		INNER JOIN public.tier_pricing tp ON tp.tier = s.tier
		WHERE s.agent_id = $1 AND lower(s.status) IN ('pending','running')
	`, agentID).Scan(&max)
	return max, err
}

func (s *Store) ListSandboxesByUser(ctx context.Context, userID uuid.UUID, limit int) ([]Sandbox, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.Tx.Query(ctx, `SELECT `+sandboxColumns+`
		FROM public.sandboxes WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSandboxRows(rows)
}

func (s *Store) ListSandboxesByAgent(ctx context.Context, agentID uuid.UUID, limit int) ([]Sandbox, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.Tx.Query(ctx, `SELECT `+sandboxColumns+`
		FROM public.sandboxes WHERE agent_id = $1 ORDER BY created_at DESC LIMIT $2`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSandboxRows(rows)
}

func scanSandboxRows(rows pgx.Rows) ([]Sandbox, error) {
	var out []Sandbox
	for rows.Next() {
		var sb Sandbox
		if err := scanSandbox(rows, &sb); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}
