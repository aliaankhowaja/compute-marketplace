package payment

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/payment/lnbot"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// SandboxDepositObserver is invoked after a sandbox-scoped pending deposit settles so the
// orchestrator can boot the pod (initial prepay) or resume it (mid-run topup).
// newBalanceMsats is the post-credit prepaid balance.
type SandboxDepositObserver func(ctx context.Context, sandboxID uuid.UUID, amountMsats, newBalanceMsats int64)

// DepositSettlerBackground polls pending manual BOLT11 deposits and confirms settlement via LNBOT.
type DepositSettlerBackground struct {
	Store            *store.Store
	LNBot            *lnbot.Client
	Interval         time.Duration
	OnSandboxSettled SandboxDepositObserver
}

// Run blocks until ctx is cancelled.
func (w *DepositSettlerBackground) Run(ctx context.Context) {
	if w == nil || w.Store == nil || w.LNBot == nil {
		return
	}
	tick := w.Interval
	if tick <= 0 {
		tick = 12 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.pollOnce(ctx)
		}
	}
}

func decodeInvoicePreimage(phHex string, rawPre string) ([]byte, bool) {
	rawPre = strings.TrimSpace(rawPre)
	if rawPre == "" {
		return nil, false
	}
	preimage, err := hex.DecodeString(rawPre)
	if err != nil || len(preimage) != 32 {
		slog.Warn("lnbot invoice preimage decode failed", "payment_hash", phHex, "err", err)
		return nil, false
	}
	return preimage, true
}

func (w *DepositSettlerBackground) pollOnce(ctx context.Context) {
	rows, err := w.Store.ListPendingDepositRows(ctx, 20)
	if err != nil {
		slog.Warn("pending deposits list", "err", err)
		return
	}
	for _, r := range rows {
		phHex := hex.EncodeToString(r.PayHash)
		inv, err := w.LNBot.GetInvoiceByPaymentHash(ctx, phHex)
		if err != nil {
			slog.Warn("lnbot get invoice", "payment_hash", phHex, "err", err)
			continue
		}
		if inv == nil {
			continue
		}
		if strings.TrimSpace(strings.ToLower(inv.Status)) != "settled" {
			continue
		}

		preimage, preimageOK := decodeInvoicePreimage(phHex, inv.PreimageHex)
		if !preimageOK && r.AgentID != nil {
			slog.Debug("lnbot settled agent deposit without usable preimage (will settle with NULL preimage)", "payment_hash", phHex)
		}

		dep := store.PendingDepositRow{
			ID:        r.ID,
			AgentID:   r.AgentID,
			SandboxID: r.SandboxID,
			PayHash:   r.PayHash,
			Invoice:   r.Invoice,
			Amount:    r.Amount,
		}

		switch {
		case r.SandboxID != nil:
			newBal, err := w.Store.SettleSandboxDeposit(ctx, dep, preimage, r.Invoice)
			if err != nil {
				slog.Warn("settle sandbox deposit", "id", r.ID, "err", err)
				continue
			}
			if !preimageOK {
				slog.Info("sandbox deposit settled without preimage from LNBOT", "sandbox_id", r.SandboxID.String(), "payment_hash", phHex)
			}
			if w.OnSandboxSettled != nil {
				w.OnSandboxSettled(ctx, *r.SandboxID, r.Amount, newBal)
			}
		case r.AgentID != nil:
			if err := w.Store.SettlePendingDeposit(ctx, dep, preimage, r.Invoice); err != nil {
				slog.Warn("settle pending deposit", "id", r.ID, "err", err)
			}
		default:
			slog.Warn("pending deposit has neither agent nor sandbox", "id", r.ID)
		}
	}
}
