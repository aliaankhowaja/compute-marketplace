package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const sandboxPoolFloorSubcents = -10_000_000

func (s *Store) GetUserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	q := `SELECT id, email, credit_balance_subcents, warning_level FROM public.users WHERE id = $1`
	var u User
	err := s.Tx.QueryRow(ctx, q, id).Scan(&u.ID, &u.Email, &u.CreditBalanceSubcents, &u.WarningLevel)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) AddUserCredits(ctx context.Context, userID uuid.UUID, amountSubcents int64, txType, desc string, stripePI *string) (balanceAfter int64, err error) {
	tx, err := s.Session.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cur int64
	if err := tx.QueryRow(ctx, `SELECT credit_balance_subcents FROM public.users WHERE id = $1 FOR UPDATE`, userID).Scan(&cur); err != nil {
		return 0, err
	}
	newBal := cur + amountSubcents
	if newBal < 0 {
		return 0, fmt.Errorf("insufficient credits")
	}
	if _, err := tx.Exec(ctx, `UPDATE public.users SET credit_balance_subcents = $2 WHERE id = $1`, userID, newBal); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.credit_transactions (user_id, sandbox_id, type, amount_subcents, balance_after_subcents, description, stripe_payment_intent_id)
		VALUES ($1, NULL, $2, $3, $4, $5, $6)`,
		userID, txType, amountSubcents, newBal, desc, stripePI)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return newBal, nil
}

// AllocateCreditsForSandbox pulls prepaid credits from the user balance into the per-sandbox pool (session pool + advisory lock).
func (s *Store) AllocateCreditsForSandbox(ctx context.Context, userID, sandboxID uuid.UUID, requestedSubcents int64) (allocated int64, sandboxPoolAfter int64, userBalanceAfter int64, err error) {
	tx, err := s.Session.Begin(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text))`, userID.String()); err != nil {
		return 0, 0, 0, err
	}
	var bal int64
	if err := tx.QueryRow(ctx, `SELECT credit_balance_subcents FROM public.users WHERE id = $1 FOR UPDATE`, userID).Scan(&bal); err != nil {
		return 0, 0, 0, err
	}
	var alloc int64
	if bal >= requestedSubcents {
		alloc = requestedSubcents
	} else if bal > 0 {
		alloc = bal
	} else {
		return 0, 0, bal, fmt.Errorf("insufficient credits")
	}
	newBal := bal - alloc
	if _, err := tx.Exec(ctx, `UPDATE public.users SET credit_balance_subcents = $2 WHERE id = $1`, userID, newBal); err != nil {
		return 0, 0, 0, err
	}

	var poolAfter int64
	if err := tx.QueryRow(ctx,
		`UPDATE public.sandboxes SET prepaid_balance_subcents = prepaid_balance_subcents + $2 WHERE id = $1 RETURNING prepaid_balance_subcents`,
		sandboxID, alloc).Scan(&poolAfter); err != nil {
		return 0, 0, 0, err
	}

	_, err = tx.Exec(ctx, `INSERT INTO public.credit_transactions (user_id, sandbox_id, type, amount_subcents, balance_after_subcents, description)
		VALUES ($1, $2, 'allocation', $3, $4, $5)`,
		userID, sandboxID, alloc, newBal, "runtime allocation chunk")
	if err != nil {
		return 0, 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, err
	}
	return alloc, poolAfter, newBal, nil
}

// RecordCreditComputeCharges writes compute_charge rows and debits the per-sandbox credit pool.
// All charges must share the same SandboxID. Uses batched writes to minimize round-trips.
func (s *Store) RecordCreditComputeCharges(ctx context.Context, charges []CreditCharge) error {
	if len(charges) == 0 {
		return nil
	}
	tx, err := s.Session.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sandboxID := charges[0].SandboxID

	var poolBal int64
	if err := tx.QueryRow(ctx,
		`SELECT prepaid_balance_subcents FROM public.sandboxes WHERE id = $1 FOR UPDATE`,
		sandboxID).Scan(&poolBal); err != nil {
		return err
	}

	type resolved struct {
		ch       CreditCharge
		balAfter int64
		desc     string
	}
	items := make([]resolved, 0, len(charges))
	runningBal := poolBal
	for _, ch := range charges {
		if ch.AmountSubcents <= 0 {
			continue
		}
		runningBal -= ch.AmountSubcents
		if runningBal < sandboxPoolFloorSubcents {
			runningBal = sandboxPoolFloorSubcents
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
		items = append(items, resolved{ch: ch, balAfter: runningBal, desc: desc})
	}

	if len(items) == 0 {
		return tx.Commit(ctx)
	}

	batch := &pgx.Batch{}
	for _, it := range items {
		batch.Queue(`INSERT INTO public.credit_transactions (user_id, sandbox_id, type, amount_subcents, balance_after_subcents, description)
			VALUES ($1,$2,'compute_charge',$3,$4,$5)`,
			it.ch.UserID, it.ch.SandboxID, it.ch.AmountSubcents, it.balAfter, it.desc)
	}
	finalBal := items[len(items)-1].balAfter
	batch.Queue(`UPDATE public.sandboxes SET prepaid_balance_subcents = $2 WHERE id = $1`,
		sandboxID, finalBal)

	br := tx.SendBatch(ctx, batch)
	if err := br.Close(); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// RefundSandboxCredits returns unused sandbox pool credits to the user balance on delete.
func (s *Store) RefundSandboxCredits(ctx context.Context, userID, sandboxID uuid.UUID) (refundedSubcents int64, err error) {
	tx, err := s.Session.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1::text))`, userID.String()); err != nil {
		return 0, err
	}

	var pool int64
	if err := tx.QueryRow(ctx,
		`SELECT prepaid_balance_subcents FROM public.sandboxes WHERE id = $1 FOR UPDATE`,
		sandboxID).Scan(&pool); err != nil {
		return 0, err
	}
	if pool <= 0 {
		return 0, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE public.sandboxes SET prepaid_balance_subcents = 0 WHERE id = $1`,
		sandboxID); err != nil {
		return 0, err
	}

	var newBal int64
	if err := tx.QueryRow(ctx,
		`UPDATE public.users SET credit_balance_subcents = credit_balance_subcents + $2 WHERE id = $1 RETURNING credit_balance_subcents`,
		userID, pool).Scan(&newBal); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO public.credit_transactions (user_id, sandbox_id, type, amount_subcents, balance_after_subcents, description)
		VALUES ($1, $2, 'refund', $3, $4, 'sandbox refund on delete')`,
		userID, sandboxID, pool, newBal); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return pool, nil
}

// SetUserWarningLevel updates dashboard warning.
func (s *Store) SetUserWarningLevel(ctx context.Context, userID uuid.UUID, level string) error {
	_, err := s.Tx.Exec(ctx, `UPDATE public.users SET warning_level = $2 WHERE id = $1`, userID, level)
	return err
}
