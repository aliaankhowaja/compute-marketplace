package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateAgent(ctx context.Context, jwtSub uuid.UUID, offerType string, bolt12 *string, lnAddr *string) (*Agent, error) {
	id := uuid.New()
	q := `INSERT INTO public.agents (id, jwt_sub, offer_type, bolt12_offer, lightning_address, balance_msats, warning_level)
		VALUES ($1,$2,$3,$4,$5,0,'ok')
		RETURNING id, jwt_sub, offer_type, bolt12_offer, lightning_address, balance_msats, warning_level`
	var a Agent
	err := s.Tx.QueryRow(ctx, q, id, jwtSub, offerType, bolt12, lnAddr).Scan(
		&a.ID, &a.JWTSub, &a.OfferType, &a.Bolt12Offer, &a.LightningAddress, &a.BalanceMsats, &a.WarningLevel,
	)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) GetAgentByID(ctx context.Context, id uuid.UUID) (*Agent, error) {
	q := `SELECT id, jwt_sub, offer_type, bolt12_offer, lightning_address, balance_msats, warning_level
		FROM public.agents WHERE id = $1`
	var a Agent
	err := s.Tx.QueryRow(ctx, q, id).Scan(
		&a.ID, &a.JWTSub, &a.OfferType, &a.Bolt12Offer, &a.LightningAddress, &a.BalanceMsats, &a.WarningLevel,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) GetAgentByJWTSub(ctx context.Context, sub uuid.UUID) (*Agent, error) {
	q := `SELECT id, jwt_sub, offer_type, bolt12_offer, lightning_address, balance_msats, warning_level
		FROM public.agents WHERE jwt_sub = $1`
	var a Agent
	err := s.Tx.QueryRow(ctx, q, sub).Scan(
		&a.ID, &a.JWTSub, &a.OfferType, &a.Bolt12Offer, &a.LightningAddress, &a.BalanceMsats, &a.WarningLevel,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) UpdateAgentOffer(ctx context.Context, id uuid.UUID, offerType string, bolt12 *string, lnAddr *string) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.agents SET offer_type = $2, bolt12_offer = $3, lightning_address = $4 WHERE id = $1`,
		id, offerType, bolt12, lnAddr)
	return err
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
