package payment

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/aliaankhowaja/sockt-backend/internal/payment/lnbot"
)

const (
	// MVPL402PoolTarget mirrors MVPplans /05-payment-module.md (invoice pool target 100).
	MVPL402PoolTarget = 100
	// MVPL402PoolRefillBelow — refill pool when size drops under 40.
	MVPL402PoolRefillBelow = 40
	l402PoolMemo           = "L402-pool"
	defaultPoolInvoiceTTL  = 3600 * time.Second
)

// LNBotInvoiceIssuer mints pool invoices via LNBOT and derives payment hashes via local BOLT11 decode.
type LNBotInvoiceIssuer struct {
	Client *lnbot.Client
	Net    *chaincfg.Params
}

func (w *LNBotInvoiceIssuer) MintBolt11(ctx context.Context, amountSats int64, memo string, _ int64) (bolt11 string, rhash []byte, err error) {
	bolt11, err = w.Client.CreateInvoice(ctx, amountSats, memo, "")
	if err != nil {
		return "", nil, err
	}
	net := w.Net
	if net == nil {
		net = &chaincfg.MainNetParams
	}
	ph, err := DecodeBolt11(bolt11, net)
	if err != nil {
		return "", nil, err
	}
	return bolt11, ph, nil
}

func (w *LNBotInvoiceIssuer) CancelInvoice(ctx context.Context, paymentHash []byte) error {
	return w.Client.CancelInvoice(ctx, paymentHash)
}

type invoiceIssuer interface {
	MintBolt11(ctx context.Context, amountSats int64, memo string, expirySeconds int64) (bolt11 string, rhash []byte, err error)
	CancelInvoice(ctx context.Context, paymentHash []byte) error
}

// InvoicePool maintains warm 1‑sat invoices for L402 challenges.
type InvoicePool struct {
	mu          sync.Mutex
	pending     []*PoolEntry
	client      invoiceIssuer
	target      int
	refillBelow int
}

// NewInvoicePool wires the pool defaults for the MVP sizing policy.
func NewInvoicePool(client invoiceIssuer) *InvoicePool {
	return &InvoicePool{
		client:      client,
		target:      MVPL402PoolTarget,
		refillBelow: MVPL402PoolRefillBelow,
		pending:     make([]*PoolEntry, 0, MVPL402PoolTarget),
	}
}

// Size returns queued warm invoices.
func (p *InvoicePool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

func (p *InvoicePool) SetSizing(target, refillBelow int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if target > 0 {
		p.target = target
	}
	if refillBelow > 0 {
		p.refillBelow = refillBelow
	}
}

// Run keeps the invoice pool warm until ctx is canceled.
func (p *InvoicePool) Run(ctx context.Context) {
	tick := time.NewTicker(750 * time.Millisecond)
	eviction := time.NewTicker(45 * time.Second)
	defer tick.Stop()
	defer eviction.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.refill(ctx)
		case <-eviction.C:
			p.evictExpired(ctx)
		}
	}
}

func (p *InvoicePool) refill(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		cur := len(p.pending)
		if cur >= p.refillBelow {
			p.mu.Unlock()
			return
		}
		need := p.target - cur
		p.mu.Unlock()
		for i := 0; i < need && ctx.Err() == nil; i++ {
			if err := p.mintIntoPool(ctx); err != nil {
				log.Printf("payment.invoice_pool: mint error: %v", err)
				return
			}
			p.mu.Lock()
			ok := len(p.pending) >= p.refillBelow
			p.mu.Unlock()
			if ok {
				return
			}
		}
	}
}

func (p *InvoicePool) mintIntoPool(ctx context.Context) error {
	expiresAt := time.Now().Add(defaultPoolInvoiceTTL)
	bolt11, rhash, err := p.client.MintBolt11(ctx, 1, l402PoolMemo, int64(defaultPoolInvoiceTTL/time.Second))
	if err != nil {
		return err
	}
	entry := &PoolEntry{
		BOLT11:      bolt11,
		PaymentHash: append([]byte(nil), rhash...),
		Preimage:    nil,
		ExpiresAt:   expiresAt,
	}
	p.mu.Lock()
	p.pending = append(p.pending, entry)
	p.mu.Unlock()
	return nil
}

func (p *InvoicePool) evictExpired(ctx context.Context) {
	now := time.Now()
	var keep []*PoolEntry
	p.mu.Lock()
	for _, ent := range p.pending {
		if now.After(ent.ExpiresAt) {
			_ = p.client.CancelInvoice(ctx, ent.PaymentHash)
			continue
		}
		keep = append(keep, ent)
	}
	p.pending = keep
	p.mu.Unlock()
}

// Pop returns the next usable pool entry discarding stale entries eagerly.
func (p *InvoicePool) Pop(ctx context.Context) (*PoolEntry, bool) {
	for {
		p.mu.Lock()
		if len(p.pending) == 0 {
			p.mu.Unlock()
			return nil, false
		}
		ent := p.pending[0]
		p.pending = p.pending[1:]
		p.mu.Unlock()
		if ctx.Err() != nil {
			return nil, false
		}
		if time.Now().After(ent.ExpiresAt) {
			_ = p.client.CancelInvoice(ctx, ent.PaymentHash)
			continue
		}
		return ent, true
	}
}
