package payment

import (
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/aliaankhowaja/sockt-backend/internal/payment/lnbot"
	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// DepositSettlerBackground polls pending manual BOLT11 deposits and confirms settlement via LNBOT.
type DepositSettlerBackground struct {
	Store    *store.Store
	LNBot    *lnbot.Client
	Interval time.Duration
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

func (w *DepositSettlerBackground) pollOnce(ctx context.Context) {
	rows, err := w.Store.ListPendingDepositRows(ctx, 20)
	if err != nil {
		slog.Warn("pending deposits list", "err", err)
		return
	}
	for _, r := range rows {
		phHex := hex.EncodeToString(r.PayHash)
		inv, err := w.LNBot.GetInvoiceByPaymentHash(ctx, phHex)
		if err != nil || inv == nil {
			continue
		}
		if inv.Status != "settled" {
			continue
		}
		rawPre := strings.TrimSpace(inv.PreimageHex)
		if rawPre == "" {
			continue
		}
		preimage, err := hex.DecodeString(rawPre)
		if err != nil || len(preimage) != 32 {
			continue
		}
		if err := w.Store.SettlePendingDeposit(ctx, store.PendingDepositRow{
			ID:      r.ID,
			AgentID: r.AgentID,
			PayHash: r.PayHash,
			Invoice: r.Invoice,
			Amount:  r.Amount,
		}, preimage, r.Invoice); err != nil {
			slog.Warn("settle pending deposit", "id", r.ID, "err", err)
		}
	}
}
