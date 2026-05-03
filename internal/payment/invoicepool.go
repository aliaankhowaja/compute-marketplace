package payment

import (
	"context"
	"crypto/rand"
	"log"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	// MVPL402PoolTarget mirrors MVPplans /05-payment-module.md (invoice pool target 100).
	MVPL402PoolTarget = 100
	// MVPL402PoolRefillBelow — refill pool when size drops under 40.
	MVPL402PoolRefillBelow = 40
	l402PoolMemo           = "L402-pool"
	l402SatMsat            = int64(1000) // 1 sat
	defaultPoolInvoiceTTL  = 3600 * time.Second
)

type invoiceIssuer interface {
	AddMintedInvoice(ctx context.Context, inv *lnrpc.Invoice) (*lnrpc.AddInvoiceResponse, error)
	CancelInvoice(ctx context.Context, paymentHash []byte) error
}

// InvoicePool maintains warm 1‑sat preimage invoices for L402 challenges.
type InvoicePool struct {
	mu          sync.Mutex
	pending     []*PoolEntry
	client      invoiceIssuer
	target      int
	refillBelow int
}

// NewInvoicePool wires the pool defaults for the MVP sizing policy.
func NewInvoicePool(client *Client) *InvoicePool {
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
	preimage := make([]byte, 32)
	if _, err := rand.Read(preimage); err != nil {
		return err
	}
	expiresAt := time.Now().Add(defaultPoolInvoiceTTL)
	resp, err := p.client.AddMintedInvoice(ctx, &lnrpc.Invoice{
		RPreimage: preimage,
		ValueMsat: l402SatMsat,
		Memo:      l402PoolMemo,
		Expiry:    int64(defaultPoolInvoiceTTL / time.Second),
	})
	if err != nil {
		return err
	}
	entry := &PoolEntry{
		BOLT11:      resp.GetPaymentRequest(),
		PaymentHash: append([]byte(nil), resp.GetRHash()...),
		Preimage:    append([]byte(nil), preimage...),
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
