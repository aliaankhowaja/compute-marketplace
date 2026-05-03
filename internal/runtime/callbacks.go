package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AuditInterval is one second of metering shipped to the control plane.
type AuditInterval struct {
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	RatePerSecond int64     `json:"rate_per_second"`
	Consumed      int64     `json:"consumed"`
	BillingMethod string    `json:"billing_method"`
}

type auditBatchPayload struct {
	SandboxID     string          `json:"sandbox_id"`
	PodID         string          `json:"pod_id"`
	BillingMethod string          `json:"billing_method"`
	Intervals     []AuditInterval `json:"intervals"`
}

type triggerPayload struct {
	SandboxID      string `json:"sandbox_id"`
	PodID          string `json:"pod_id"`
	BillingMethod  string `json:"billing_method"`
	Balance        int64  `json:"balance"`
	ConsumedTotal  int64  `json:"consumed_total"`
	Reason         string `json:"reason,omitempty"`
	RequestedTopup int64  `json:"requested_topup,omitempty"`
}

// Callbacks posts billing lifecycle signals to the control plane with retries.
type Callbacks struct {
	cfg    *Config
	client *http.Client

	mu       sync.Mutex
	failures int
}

// NewCallbacks constructs a Callbacks helper.
func NewCallbacks(cfg *Config) *Callbacks {
	return &Callbacks{
		cfg: cfg,
		client: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

// FailureCount returns consecutive callback failures (best-effort diagnostic).
func (c *Callbacks) FailureCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures
}

func (c *Callbacks) bumpFailure() {
	c.mu.Lock()
	c.failures++
	c.mu.Unlock()
}

func (c *Callbacks) resetFailure() {
	c.mu.Lock()
	c.failures = 0
	c.mu.Unlock()
}

func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	path = strings.TrimLeft(path, "/")
	return base + "/" + path
}

func (c *Callbacks) postJSON(ctx context.Context, urlPath string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	url := joinURL(c.cfg.ControlPlaneURL, urlPath)

	var lastErr error
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff = time.Duration(float64(backoff) * 1.8)
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if c.cfg.CallbackSecret != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.CallbackSecret)
		}

		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			c.bumpFailure()
			continue
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			c.resetFailure()
			return nil
		}

		lastErr = fmt.Errorf("callback %s failed: status=%d", urlPath, resp.StatusCode)
		c.bumpFailure()
	}

	if lastErr == nil {
		lastErr = errors.New("callback failed")
	}
	return lastErr
}

// PostAuditBatch sends accumulated audit intervals.
func (c *Callbacks) PostAuditBatch(ctx context.Context, intervals []AuditInterval) error {
	if len(intervals) == 0 {
		return nil
	}
	body := auditBatchPayload{
		SandboxID:     c.cfg.SandboxID,
		PodID:         c.cfg.PodID,
		BillingMethod: c.cfg.BillingMethod,
		Intervals:     intervals,
	}
	return c.postJSON(ctx, "v1/runtime/billing/audit-batch", body)
}

// PostPauseTrigger asks the control plane to pause the sandbox (out of balance).
func (c *Callbacks) PostPauseTrigger(ctx context.Context, balance, consumedTotal int64, reason string) error {
	body := triggerPayload{
		SandboxID:     c.cfg.SandboxID,
		PodID:         c.cfg.PodID,
		BillingMethod: c.cfg.BillingMethod,
		Balance:       balance,
		ConsumedTotal: consumedTotal,
		Reason:        reason,
	}
	return c.postJSON(ctx, "v1/runtime/pause-trigger", body)
}

type allocateResponse struct {
	BalanceDelta int64 `json:"balance_delta"`
	NewBalance   int64 `json:"new_balance"`
	ErrorClass   string `json:"error_class,omitempty"`
}

// PostCreditAllocateApply requests additional prepaid credits from the control plane (credits path only).
func (c *Callbacks) PostCreditAllocateApply(ctx context.Context, balance, consumedTotal int64, apply func(delta int64, newBal int64)) error {
	payload, err := json.Marshal(triggerPayload{
		SandboxID:      c.cfg.SandboxID,
		PodID:          c.cfg.PodID,
		BillingMethod:  c.cfg.BillingMethod,
		Balance:        balance,
		ConsumedTotal:  consumedTotal,
		RequestedTopup: c.cfg.TopupAmount,
	})
	if err != nil {
		return err
	}

	url := joinURL(c.cfg.ControlPlaneURL, "v1/runtime/topup-trigger")

	var lastErr error
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff = time.Duration(float64(backoff) * 1.8)
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if c.cfg.CallbackSecret != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.CallbackSecret)
		}

		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			c.bumpFailure()
			continue
		}

		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			c.resetFailure()
			if apply != nil {
				var tr allocateResponse
				_ = json.Unmarshal(bodyBytes, &tr)
				switch {
				case tr.NewBalance != 0:
					apply(0, tr.NewBalance)
				case tr.BalanceDelta != 0:
					apply(tr.BalanceDelta, 0)
				default:
					apply(0, 0)
				}
			}
			return nil
		}

		lastErr = fmt.Errorf("callback topup-trigger failed: status=%d", resp.StatusCode)
		c.bumpFailure()
	}

	return lastErr
}

// RuntimeDepositMint is a BOLT11 deposit invoice minted when the sandbox requests prepaid funds over Lightning.
type RuntimeDepositMint struct {
	Bolt11        string `json:"bolt11"`
	PaymentHash   string `json:"payment_hash"`
	AmountMsats   int64  `json:"amount_msats"`
	ExpiresAt     string `json:"expires_at"`
	WarningLevel  string `json:"warning_level,omitempty"`
	ErrorClass    string `json:"error_class,omitempty"`
}

type runtimeMintEnvelope struct {
	ErrorClass string `json:"error_class"`
	Deposit    struct {
		Bolt11      string `json:"bolt11"`
		PaymentHash string `json:"payment_hash"`
		AmountMsats int64  `json:"amount_msats"`
		ExpiresAt   string `json:"expires_at"`
	} `json:"deposit"`
	WarningLevel string `json:"warning_level,omitempty"`
}

// PostRuntimeDepositMint asks the control plane to mint a deposit invoice (lightning path only).
func (c *Callbacks) PostRuntimeDepositMint(ctx context.Context, balance, consumedTotal int64) (*RuntimeDepositMint, error) {
	payload, err := json.Marshal(triggerPayload{
		SandboxID:      c.cfg.SandboxID,
		PodID:          c.cfg.PodID,
		BillingMethod:  c.cfg.BillingMethod,
		Balance:        balance,
		ConsumedTotal:  consumedTotal,
		RequestedTopup: c.cfg.TopupAmount,
	})
	if err != nil {
		return nil, err
	}

	url := joinURL(c.cfg.ControlPlaneURL, "v1/runtime/topup-trigger")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.CallbackSecret != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.CallbackSecret)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.bumpFailure()
		return nil, err
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.bumpFailure()
		return nil, fmt.Errorf("runtime deposit mint callback status=%d", resp.StatusCode)
	}
	c.resetFailure()
	var raw runtimeMintEnvelope
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw.Deposit.Bolt11) == "" {
		return nil, fmt.Errorf("missing deposit in response (%s)", raw.ErrorClass)
	}
	return &RuntimeDepositMint{
		Bolt11:       raw.Deposit.Bolt11,
		PaymentHash:  raw.Deposit.PaymentHash,
		AmountMsats:  raw.Deposit.AmountMsats,
		ExpiresAt:    raw.Deposit.ExpiresAt,
		WarningLevel: raw.WarningLevel,
		ErrorClass:   raw.ErrorClass,
	}, nil
}
