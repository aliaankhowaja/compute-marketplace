package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetTierPricing(ctx context.Context, tier string) (*TierPricing, error) {
	q := `SELECT tier, msats_per_second, usd_cents_per_second_x100 FROM public.tier_pricing WHERE tier = $1`
	var p TierPricing
	err := s.Tx.QueryRow(ctx, q, tier).Scan(&p.Tier, &p.MsatsPerSecond, &p.USDCentsPerSecondX100)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) ListTierPricing(ctx context.Context) ([]TierPricing, error) {
	rows, err := s.Tx.Query(ctx, `SELECT tier, msats_per_second, usd_cents_per_second_x100 FROM public.tier_pricing ORDER BY tier`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TierPricing
	for rows.Next() {
		var p TierPricing
		if err := rows.Scan(&p.Tier, &p.MsatsPerSecond, &p.USDCentsPerSecondX100); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetTemplateByName(ctx context.Context, name string) (*Template, error) {
	q := `SELECT id, name, runpod_template_id, runtime, default_tier FROM public.templates WHERE name = $1`
	var t Template
	err := s.Tx.QueryRow(ctx, q, name).Scan(&t.ID, &t.Name, &t.RunpodTemplateID, &t.Runtime, &t.DefaultTier)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := s.Tx.Query(ctx, `SELECT id, name, runpod_template_id, runtime, default_tier FROM public.templates ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Name, &t.RunpodTemplateID, &t.Runtime, &t.DefaultTier); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
