package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) InsertSandbox(ctx context.Context, sb *Sandbox) error {
	if sb == nil {
		return fmt.Errorf("nil sandbox")
	}
	if sb.ID == uuid.Nil {
		sb.ID = uuid.New()
	}
	q := `INSERT INTO public.sandboxes (
		id, agent_id, user_id, api_key_id, billing_method, tier, template, status, runtime,
		pod_id, pod_proxy_url, network_volume_id, consumed_msats
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	_, err := s.Tx.Exec(ctx, q,
		sb.ID, sb.AgentID, sb.UserID, sb.APIKeyID, sb.BillingMethod, sb.Tier, sb.Template, sb.Status, sb.Runtime,
		sb.PodID, sb.PodProxyURL, sb.NetworkVolumeID, sb.ConsumedMsats,
	)
	return err
}

func (s *Store) GetSandbox(ctx context.Context, id uuid.UUID) (*Sandbox, error) {
	q := `SELECT id, agent_id, user_id, api_key_id, billing_method, tier, template, status, runtime,
		pod_id, pod_proxy_url, network_volume_id, COALESCE(consumed_msats, 0)
		FROM public.sandboxes WHERE id = $1`
	var sb Sandbox
	err := s.Tx.QueryRow(ctx, q, id).Scan(
		&sb.ID, &sb.AgentID, &sb.UserID, &sb.APIKeyID, &sb.BillingMethod, &sb.Tier, &sb.Template, &sb.Status, &sb.Runtime,
		&sb.PodID, &sb.PodProxyURL, &sb.NetworkVolumeID, &sb.ConsumedMsats,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sb, nil
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

func (s *Store) ListSandboxesByUser(ctx context.Context, userID uuid.UUID, limit int) ([]Sandbox, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.Tx.Query(ctx, `SELECT id, agent_id, user_id, api_key_id, billing_method, tier, template, status, runtime,
		pod_id, pod_proxy_url, network_volume_id, COALESCE(consumed_msats, 0)
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
	rows, err := s.Tx.Query(ctx, `SELECT id, agent_id, user_id, api_key_id, billing_method, tier, template, status, runtime,
		pod_id, pod_proxy_url, network_volume_id, COALESCE(consumed_msats, 0)
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
		if err := rows.Scan(
			&sb.ID, &sb.AgentID, &sb.UserID, &sb.APIKeyID, &sb.BillingMethod, &sb.Tier, &sb.Template, &sb.Status, &sb.Runtime,
			&sb.PodID, &sb.PodProxyURL, &sb.NetworkVolumeID, &sb.ConsumedMsats,
		); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}
