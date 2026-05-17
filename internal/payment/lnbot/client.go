// Package lnbot is a minimal HTTP client for the LNBOT REST API (https://ln.bot/docs/api).
package lnbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.ln.bot"

// HTTPError is returned for non-success HTTP responses from the API (use errors.As).
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("lnbot: HTTP %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("lnbot: HTTP %d", e.StatusCode)
}

// PaymentFailedError is returned when a payment completes with status failed.
type PaymentFailedError struct {
	FailureReason string
}

func (e *PaymentFailedError) Error() string {
	if e.FailureReason != "" {
		return e.FailureReason
	}
	return "payment failed"
}

// Client calls api.ln.bot with a user key (uk_…) or wallet key (wk_…).
type Client struct {
	baseURL    string
	apiKey     string
	walletID   string
	httpClient *http.Client
}

// New constructs a client. baseURL may be empty (defaults to production). walletID may be empty to use /v1/wallets/current with a wallet key.
func New(baseURL, apiKey, walletID string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	b := strings.TrimSpace(strings.TrimSuffix(baseURL, "/"))
	if b == "" {
		b = defaultBaseURL
	}
	return &Client{
		baseURL:    b,
		apiKey:     strings.TrimSpace(apiKey),
		walletID:   strings.TrimSpace(walletID),
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (c *Client) walletsPrefix() string {
	if c.walletID != "" {
		return "/v1/wallets/" + c.walletID
	}
	return "/v1/wallets/current"
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody any, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if respBody != nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, respBody); err != nil {
			return fmt.Errorf("lnbot decode: %w", err)
		}
	}
	return nil
}

// MeResponse is GET /v1/me.
type MeResponse struct {
	WalletID string `json:"walletId"`
}

// Me returns identity for the current API key.
func (c *Client) Me(ctx context.Context) (*MeResponse, error) {
	var out MeResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type createInvoiceReq struct {
	Amount    int64  `json:"amount"`
	Memo      string `json:"memo,omitempty"`
	Reference string `json:"reference,omitempty"`
}

type invoiceJSON struct {
	Status      string  `json:"status"`
	Amount      int64   `json:"amount"`
	Bolt11      string  `json:"bolt11"`
	Memo        string  `json:"memo"`
	Preimage    *string `json:"preimage"`
	PaymentHash string  `json:"paymentHash"`
}

// CreateInvoice calls POST …/invoices. Amount is satoshis. API invoice lifetime is fixed server-side (~60 minutes).
func (c *Client) CreateInvoice(ctx context.Context, amountSats int64, memo, reference string) (bolt11 string, err error) {
	var resp invoiceJSON
	req := createInvoiceReq{Amount: amountSats, Memo: memo, Reference: reference}
	path := c.walletsPrefix() + "/invoices"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &resp); err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Bolt11), nil
}

// Invoice is a wallet invoice returned by GET …/invoices/{paymentHash}.
type Invoice struct {
	Status      string
	Amount      int64
	Bolt11      string
	PreimageHex string
}

// GetInvoiceByPaymentHash loads invoice status by payment hash (hex, no 0x prefix).
func (c *Client) GetInvoiceByPaymentHash(ctx context.Context, paymentHashHex string) (*Invoice, error) {
	path := fmt.Sprintf("%s/invoices/%s", c.walletsPrefix(), strings.TrimSpace(strings.TrimPrefix(paymentHashHex, "0x")))
	var resp invoiceJSON
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	out := &Invoice{
		Status: strings.TrimSpace(strings.ToLower(resp.Status)),
		Amount: resp.Amount,
		Bolt11: strings.TrimSpace(resp.Bolt11),
	}
	if resp.Preimage != nil {
		out.PreimageHex = strings.TrimSpace(*resp.Preimage)
	}
	return out, nil
}

type paymentReq struct {
	Target         string `json:"target"`
	Amount         *int64 `json:"amount,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	Reference      string `json:"reference,omitempty"`
}

type paymentJSON struct {
	Status        string  `json:"status"`
	Preimage      string  `json:"preimage"`
	FailureReason *string `json:"failureReason"`
}

// SendPayment pays a BOLT11 (or address/LNURL) via POST …/payments.
func (c *Client) SendPayment(ctx context.Context, bolt11 string, idempotencyKey string) (preimageHex string, err error) {
	req := paymentReq{
		Target:         strings.TrimSpace(bolt11),
		IdempotencyKey: strings.TrimSpace(idempotencyKey),
	}
	var resp paymentJSON
	path := c.walletsPrefix() + "/payments"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &resp); err != nil {
		return "", err
	}
	switch strings.TrimSpace(strings.ToLower(resp.Status)) {
	case "settled":
		ph := strings.TrimSpace(resp.Preimage)
		if ph == "" {
			return "", fmt.Errorf("lnbot: settled payment missing preimage")
		}
		return ph, nil
	case "failed":
		reason := ""
		if resp.FailureReason != nil {
			reason = strings.TrimSpace(*resp.FailureReason)
		}
		return "", &PaymentFailedError{FailureReason: reason}
	default:
		return "", fmt.Errorf("lnbot: unexpected payment status %q", resp.Status)
	}
}

// SendToAddress pays a lightning address (or LNURL) a specified amount in sats.
func (c *Client) SendToAddress(ctx context.Context, address string, amountSats int64, idempotencyKey string) error {
	req := paymentReq{
		Target:         strings.TrimSpace(address),
		Amount:         &amountSats,
		IdempotencyKey: strings.TrimSpace(idempotencyKey),
	}
	var resp paymentJSON
	path := c.walletsPrefix() + "/payments"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &resp); err != nil {
		return err
	}
	switch strings.TrimSpace(strings.ToLower(resp.Status)) {
	case "settled", "pending":
		return nil
	case "failed":
		reason := ""
		if resp.FailureReason != nil {
			reason = strings.TrimSpace(*resp.FailureReason)
		}
		return &PaymentFailedError{FailureReason: reason}
	default:
		return fmt.Errorf("lnbot: unexpected payment status %q", resp.Status)
	}
}

// WalletDetails is GET …/wallets/current or GET …/wallets/{walletId}.
type WalletDetails struct {
	WalletID  string `json:"walletId"`
	Balance   int64  `json:"balance"`
	OnHold    int64  `json:"onHold"`
	Available int64  `json:"available"`
}

// GetWallet loads custodial wallet balances.
func (c *Client) GetWallet(ctx context.Context) (*WalletDetails, error) {
	path := c.walletsPrefix()
	var w WalletDetails
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// CancelInvoice is a no-op: LNBOT does not document invoice cancellation; pool eviction relies on expiry only.
func (c *Client) CancelInvoice(_ context.Context, _ []byte) error {
	return nil
}
