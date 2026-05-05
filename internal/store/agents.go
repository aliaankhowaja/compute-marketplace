package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func agentJWTSubFromPG(js pgtype.UUID) *uuid.UUID {
	if !js.Valid {
		return nil
	}
	u := uuid.UUID(js.Bytes)
	return &u
}

func (s *Store) CreateAgent(ctx context.Context, jwtSub uuid.UUID) (*Agent, error) {
	id := uuid.New()
	q := `INSERT INTO public.agents (id, jwt_sub, balance_msats, warning_level)
		VALUES ($1,$2,0,'ok')
		RETURNING id, jwt_sub, balance_msats, warning_level`
	var a Agent
	var js pgtype.UUID
	err := s.Tx.QueryRow(ctx, q, id, jwtSub).Scan(&a.ID, &js, &a.BalanceMsats, &a.WarningLevel)
	if err != nil {
		return nil, err
	}
	a.JWTSub = agentJWTSubFromPG(js)
	return &a, nil
}

func (s *Store) GetAgentByID(ctx context.Context, id uuid.UUID) (*Agent, error) {
	q := `SELECT id, jwt_sub, balance_msats, warning_level
		FROM public.agents WHERE id = $1`
	var a Agent
	var js pgtype.UUID
	err := s.Tx.QueryRow(ctx, q, id).Scan(&a.ID, &js, &a.BalanceMsats, &a.WarningLevel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.JWTSub = agentJWTSubFromPG(js)
	return &a, nil
}

func (s *Store) GetAgentByJWTSub(ctx context.Context, sub uuid.UUID) (*Agent, error) {
	q := `SELECT id, jwt_sub, balance_msats, warning_level
		FROM public.agents WHERE jwt_sub = $1`
	var a Agent
	var js pgtype.UUID
	err := s.Tx.QueryRow(ctx, q, sub).Scan(&a.ID, &js, &a.BalanceMsats, &a.WarningLevel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.JWTSub = agentJWTSubFromPG(js)
	return &a, nil
}

// CreateAnonymousAgent inserts an agent row with jwt_sub NULL for MCP anonymous L402 identity.
// Idempotent on primary key id (ON CONFLICT DO NOTHING).
func (s *Store) CreateAnonymousAgent(ctx context.Context, id uuid.UUID) (*Agent, error) {
	q := `INSERT INTO public.agents (id, jwt_sub, balance_msats, warning_level)
		VALUES ($1, NULL, 0, 'ok')
		ON CONFLICT (id) DO NOTHING
		RETURNING id, jwt_sub, balance_msats, warning_level`
	var a Agent
	var js pgtype.UUID
	err := s.Tx.QueryRow(ctx, q, id).Scan(&a.ID, &js, &a.BalanceMsats, &a.WarningLevel)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.GetAgentByID(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	a.JWTSub = agentJWTSubFromPG(js)
	return &a, nil
}

func (s *Store) IncrementAgentBalanceMsats(ctx context.Context, id uuid.UUID, delta int64) (int64, error) {
	q := `UPDATE public.agents SET balance_msats = balance_msats + $2 WHERE id = $1 RETURNING balance_msats`
	var bal int64
	err := s.Tx.QueryRow(ctx, q, id, delta).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("agent not found")
	}
	return bal, err
}

func (s *Store) DecrementAgentBalanceMsats(ctx context.Context, id uuid.UUID, delta int64) (int64, error) {
	return s.IncrementAgentBalanceMsats(ctx, id, -delta)
}

// RefreshAgentLightningWarning sets warning_level from balance thresholds (msats floor at 0 vs low-water).
func (s *Store) RefreshAgentLightningWarning(ctx context.Context, agentID uuid.UUID, lowWaterMsats int64) error {
	if lowWaterMsats <= 0 {
		lowWaterMsats = 1
	}
	q := `UPDATE public.agents SET warning_level =
		CASE WHEN balance_msats <= 0 THEN 'out_of_balance'
		     WHEN balance_msats < $2 THEN 'low_balance'
		     ELSE 'ok' END
		WHERE id = $1`
	_, err := s.Tx.Exec(ctx, q, agentID, lowWaterMsats)
	return err
}
