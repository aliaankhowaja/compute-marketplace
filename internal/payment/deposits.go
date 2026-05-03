package payment

import (
	"context"
	"log/slog"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"

	"github.com/aliaankhowaja/sockt-backend/internal/store"
)

// DepositSettlerBackground polls pending manual BOLT11 deposits and confirms settlement via LND.
type DepositSettlerBackground struct {
	Store    *store.Store
	LND      *Client
	Interval time.Duration
}

// Run blocks until ctx is cancelled.
func (w *DepositSettlerBackground) Run(ctx context.Context) {
	if w == nil || w.Store == nil || w.LND == nil {
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
		inv, err := w.LND.LookupInvoice(ctx, r.PayHash)
		if err != nil || inv == nil {
			continue
		}
		if inv.State != lnrpc.Invoice_SETTLED {
			continue
		}
		preimage := inv.RPreimage
		if len(preimage) != 32 {
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
