package runtime

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// RuntimeBalance tracks prepaid consumption for this sandbox pod.
type RuntimeBalance struct {
	SandboxID     string    `json:"sandbox_id"`
	BillingMethod string    `json:"billing_method"`
	RatePerSecond int64     `json:"rate_per_second"`
	Balance       int64     `json:"balance"`
	ConsumedTotal int64     `json:"consumed_total"`
	LastMeteredAt time.Time `json:"last_metered_at"`
	TopupInFlight bool      `json:"topup_in_flight"`
}

// MeterEvent is pushed to websocket subscribers for billing fan-out.
type MeterEvent struct {
	Type                  string              `json:"type"`
	Balance               int64               `json:"balance"`
	Message               string              `json:"message,omitempty"`
	SuggestedDepositMsats int64               `json:"suggested_deposit_msats,omitempty"`
	Deposit               *RuntimeDepositMint `json:"deposit,omitempty"`
}

// Meter drives the 1 Hz tick loop and coordinates callbacks + subscribers.
type Meter struct {
	cfg *Config
	cb  *Callbacks

	mu      sync.RWMutex
	balance RuntimeBalance

	pendingAudit []AuditInterval
	tickCount    int64

	// Billing is frozen after a successful pause-trigger callback (until balance tops up).
	meterHalted bool
	// oobNotified gates a single out_of_balance broadcast per low-balance episode.
	oobNotified bool
	// pauseTriggerInFlight prevents overlapping pause-trigger HTTP calls.
	pauseTriggerInFlight bool
	// nextTopupAttempt suppresses repeat topup-trigger calls while a deposit is pending.
	nextTopupAttempt time.Time

	subMu sync.RWMutex
	subs  map[chan MeterEvent]struct{}
}

// NewMeter constructs a meter using cfg initial balance.
func NewMeter(cfg *Config, cb *Callbacks) *Meter {
	m := &Meter{
		cfg: cfg,
		cb:  cb,
		balance: RuntimeBalance{
			SandboxID:     cfg.SandboxID,
			BillingMethod: cfg.BillingMethod,
			RatePerSecond: cfg.RatePerSecond,
			Balance:       cfg.InitialBalance,
			LastMeteredAt: time.Now().UTC(),
		},
		subs: make(map[chan MeterEvent]struct{}),
	}
	return m
}

// Snapshot returns a copy of local billing state.
func (m *Meter) Snapshot() RuntimeBalance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.balance
}

// ClearTopupFlight clears the credit allocation latch without changing balance.
func (m *Meter) ClearTopupFlight() {
	m.mu.Lock()
	m.balance.TopupInFlight = false
	m.mu.Unlock()
}

// ApplyBalanceDelta adjusts balance after a successful control-plane credit allocation response.
func (m *Meter) ApplyBalanceDelta(delta int64) {
	m.mu.Lock()
	m.balance.Balance += delta
	m.balance.TopupInFlight = false
	if m.balance.Balance > m.cfg.SafetyFloor {
		m.meterHalted = false
		m.oobNotified = false
	}
	b := m.balance.Balance
	m.mu.Unlock()
	m.broadcast(MeterEvent{Type: "topup", Balance: b, Message: "balance_adjusted"})
}

// ApplyNewBalance sets absolute balance when control plane returns new_balance.
func (m *Meter) ApplyNewBalance(newBal int64) {
	m.mu.Lock()
	m.balance.Balance = newBal
	m.balance.TopupInFlight = false
	if m.balance.Balance > m.cfg.SafetyFloor {
		m.meterHalted = false
		m.oobNotified = false
	}
	m.mu.Unlock()
	m.broadcast(MeterEvent{Type: "topup", Balance: newBal, Message: "balance_set"})
}

// Subscribe returns a channel receiving billing fan-out events.
func (m *Meter) Subscribe(buf int) chan MeterEvent {
	if buf <= 0 {
		buf = 8
	}
	ch := make(chan MeterEvent, buf)
	m.subMu.Lock()
	m.subs[ch] = struct{}{}
	m.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (m *Meter) Unsubscribe(ch chan MeterEvent) {
	m.subMu.Lock()
	delete(m.subs, ch)
	m.subMu.Unlock()
	close(ch)
}

func (m *Meter) broadcast(ev MeterEvent) {
	m.subMu.RLock()
	defer m.subMu.RUnlock()
	for ch := range m.subs {
		select {
		case ch <- ev:
		default:
			// drop if consumer is slow
		}
	}
}

// Start runs the 1-second metering loop until ctx is cancelled.
func (m *Meter) Start(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.flushAudit(context.Background())
			return
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

func (m *Meter) tick(parentCtx context.Context) {
	m.mu.RLock()
	halted := m.meterHalted
	m.mu.RUnlock()
	if halted {
		m.flushAudit(parentCtx)
		return
	}

	now := time.Now().UTC()

	m.mu.Lock()

	start := m.balance.LastMeteredAt
	if start.IsZero() {
		start = now.Add(-time.Second)
	}
	m.balance.LastMeteredAt = now

	rate := m.cfg.RatePerSecond
	prevBal := m.balance.Balance
	m.balance.Balance -= rate
	m.balance.ConsumedTotal += rate

	interval := AuditInterval{
		StartedAt:     start,
		EndedAt:       now,
		RatePerSecond: rate,
		Consumed:      rate,
		BillingMethod: m.cfg.BillingMethod,
	}
	m.pendingAudit = append(m.pendingAudit, interval)
	m.tickCount++

	bal := m.balance.Balance
	consumed := m.balance.ConsumedTotal
	thresh := m.cfg.TopupThreshold
	floor := m.cfg.SafetyFloor
	topupInFlight := m.balance.TopupInFlight

	m.mu.Unlock()

	m.broadcast(MeterEvent{Type: "billing", Balance: bal})

	// Topup loop (credits + lightning): when balance falls below the topup threshold and
	// we haven't already kicked the control plane within the last few seconds, ask for more.
	// Lightning replies with a BOLT11 deposit (apply leaves balance unchanged); credits replies with
	// a balance_delta or new_balance applied immediately.
	if bal < thresh && bal > floor && !topupInFlight && now.After(m.nextTopupAttempt) {
		m.mu.Lock()
		m.balance.TopupInFlight = true
		m.nextTopupAttempt = now.Add(5 * time.Second)
		m.mu.Unlock()

		if strings.EqualFold(m.cfg.BillingMethod, "lightning") && prevBal >= thresh {
			m.broadcast(MeterEvent{Type: "low_balance", Balance: bal, Message: "deposit_required", SuggestedDepositMsats: m.cfg.TopupAmount})
		}

		go func(b0, c0 int64) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			err := m.cb.PostBalanceTopupApply(ctx, b0, c0, func(tr TopupResponse) {
				if tr.Deposit != nil {
					m.broadcast(MeterEvent{
						Type:                  "deposit_required",
						Balance:               m.Snapshot().Balance,
						Message:               tr.ErrorClass,
						SuggestedDepositMsats: tr.Deposit.AmountMsats,
						Deposit:               tr.Deposit,
					})
					m.ClearTopupFlight()
					return
				}
				switch {
				case tr.NewBalance != 0:
					m.ApplyNewBalance(tr.NewBalance)
				case tr.BalanceDelta != 0:
					m.ApplyBalanceDelta(tr.BalanceDelta)
				default:
					m.ClearTopupFlight()
				}
			})
			if err != nil {
				log.Printf("runtime/meter: topup-trigger failed: %v", err)
				m.mu.Lock()
				m.balance.TopupInFlight = false
				m.mu.Unlock()
				m.broadcast(MeterEvent{Type: "error", Balance: m.Snapshot().Balance, Message: "topup_trigger_failed"})
			}
		}(bal, consumed)
	}

	if bal <= floor {
		var notifyOOB, firePause bool
		m.mu.Lock()
		if !m.oobNotified {
			m.oobNotified = true
			notifyOOB = true
		}
		if !m.pauseTriggerInFlight {
			m.pauseTriggerInFlight = true
			firePause = true
		}
		m.mu.Unlock()

		if notifyOOB {
			m.broadcast(MeterEvent{Type: "out_of_balance", Balance: bal})
		}
		if firePause {
			go func(b0, c0 int64) {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				defer func() {
					m.mu.Lock()
					m.pauseTriggerInFlight = false
					m.mu.Unlock()
				}()
				if err := m.cb.PostPauseTrigger(ctx, b0, c0, "out_of_balance"); err != nil {
					log.Printf("runtime/meter: pause-trigger failed: %v", err)
					return
				}
				m.mu.Lock()
				m.meterHalted = true
				m.mu.Unlock()
				m.flushAudit(context.Background())
			}(bal, consumed)
		}
	}

	if m.tickCount%30 == 0 {
		m.flushAudit(parentCtx)
	}
}

func (m *Meter) flushAudit(ctx context.Context) {
	m.mu.Lock()
	if len(m.pendingAudit) == 0 {
		m.mu.Unlock()
		return
	}
	batch := m.pendingAudit
	m.pendingAudit = nil
	m.mu.Unlock()

	if err := m.cb.PostAuditBatch(ctx, batch); err != nil {
		log.Printf("runtime/meter: audit-batch failed: %v", err)
		m.mu.Lock()
		m.pendingAudit = append(batch, m.pendingAudit...)
		m.mu.Unlock()
	}
}
