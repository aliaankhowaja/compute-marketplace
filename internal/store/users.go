package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) GetUserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	q := `SELECT id, email, credit_balance_usd_cents, warning_level FROM public.users WHERE id = $1`
	var u User
	err := s.Tx.QueryRow(ctx, q, id).Scan(&u.ID, &u.Email, &u.CreditBalanceUSDCents, &u.WarningLevel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) AddUserCredits(ctx context.Context, userID uuid.UUID, amountCents int64, txType, desc string, stripePI *string) (balanceAfter int64, err error) {
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cur int64
	if err := tx.QueryRow(ctx, `SELECT credit_balance_usd_cents FROM public.users WHERE id = $1 FOR UPDATE`, userID).Scan(&cur); err != nil {
		return 0, err
	}
	newBal := cur + amountCents
	if newBal < 0 {
		return 0, fmt.Errorf("insufficient credits")
	}
	if _, err := tx.Exec(ctx, `UPDATE public.users SET credit_balance_usd_cents = $2 WHERE id = $1`, userID, newBal); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.credit_transactions (user_id, sandbox_id, type, amount_usd_cents, balance_after_usd_cents, description, stripe_payment_intent_id)
		VALUES ($1, NULL, $2, $3, $4, $5, $6)`,
		userID, txType, amountCents, newBal, desc, stripePI)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return newBal, nil
}

// AllocateCreditsForSandbox pulls prepaid credits into the sandbox metering path (session pool + advisory lock).
func (s *Store) AllocateCreditsForSandbox(ctx context.Context, userID, sandboxID uuid.UUID, requestedCents int64) (allocated int64, balanceAfter int64, err error) {
	tx, err := s.Session.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text))`, userID.String()); err != nil {
		return 0, 0, err
	}
	var bal int64
	if err := tx.QueryRow(ctx, `SELECT credit_balance_usd_cents FROM public.users WHERE id = $1 FOR UPDATE`, userID).Scan(&bal); err != nil {
		return 0, 0, err
	}
	var alloc int64
	if bal >= requestedCents {
		alloc = requestedCents
	} else if bal > 0 {
		alloc = bal
	} else {
		return 0, bal, fmt.Errorf("insufficient credits")
	}
	newBal := bal - alloc
	if _, err := tx.Exec(ctx, `UPDATE public.users SET credit_balance_usd_cents = $2 WHERE id = $1`, userID, newBal); err != nil {
		return 0, 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.credit_transactions (user_id, sandbox_id, type, amount_usd_cents, balance_after_usd_cents, description)
		VALUES ($1, $2, 'allocation', $3, $4, $5)`,
		userID, sandboxID, alloc, newBal, "runtime allocation chunk")
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return alloc, newBal, nil
}

// RecordCreditComputeCharges writes compute_charge rows and debits user balance in one tx.
func (s *Store) RecordCreditComputeCharges(ctx context.Context, charges []CreditCharge) error {
	if len(charges) == 0 {
		return nil
	}
	tx, err := s.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, ch := range charges {
		if ch.AmountCents <= 0 {
			continue
		}
		var bal int64
		if err := tx.QueryRow(ctx, `SELECT credit_balance_usd_cents FROM public.users WHERE id = $1 FOR UPDATE`, ch.UserID).Scan(&bal); err != nil {
			return err
		}
		newBal := bal - ch.AmountCents
		if newBal < 0 {
			newBal = 0 // clamp; runtime should pause before negative
		}
		if _, err := tx.Exec(ctx, `UPDATE public.users SET credit_balance_usd_cents = $2 WHERE id = $1`,
			ch.UserID, newBal); err != nil {
			return err
		}
		desc := ch.Description
		if desc == "" {
			desc = "metered compute"
		}
		if ch.Tier != "" || ch.Start != nil || ch.End != nil {
			var b strings.Builder
			b.WriteString(desc)
			if ch.Tier != "" {
				b.WriteString(" tier=")
				b.WriteString(ch.Tier)
			}
			if ch.Start != nil {
				b.WriteRune(' ')
				b.WriteString(fmt.Sprint(ch.Start))
			}
			if ch.End != nil {
				b.WriteRune('-')
				b.WriteString(fmt.Sprint(ch.End))
			}
			desc = b.String()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.credit_transactions (user_id, sandbox_id, type, amount_usd_cents, balance_after_usd_cents, description)
			VALUES ($1,$2,'compute_charge',$3,$4,$5)`,
			ch.UserID, ch.SandboxID, ch.AmountCents, newBal, desc); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SetUserWarningLevel updates dashboard warning.
func (s *Store) SetUserWarningLevel(ctx context.Context, userID uuid.UUID, level string) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.users SET warning_level = $2 WHERE id = $1`, userID, level)
	return err
}
