package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LNURLClient resolves lightning addresses and LNURL-pay metadata to BOLT11.
type LNURLClient struct {
	HTTP *http.Client
}

// NewLNURLClient builds a client with the given HTTP timeout per hop.
func NewLNURLClient(httpTimeout time.Duration) *LNURLClient {
	if httpTimeout <= 0 {
		httpTimeout = 8 * time.Second
	}
	return &LNURLClient{
		HTTP: &http.Client{Timeout: httpTimeout},
	}
}

// FetchInvoice implements LNURLFetcher.
func (c *LNURLClient) FetchInvoice(ctx context.Context, lightningAddressOrLNURL string, amountSats int64) (string, error) {
	if c == nil || c.HTTP == nil {
		return "", classifyErr(ClassOfferInvalid, errors.New("lnurl client not configured"))
	}
	if amountSats <= 0 {
		return "", classifyErr(ClassOfferInvalid, errors.New("amount must be positive"))
	}
	in := strings.TrimSpace(lightningAddressOrLNURL)
	if in == "" {
		return "", classifyErr(ClassOfferInvalid, errors.New("empty lnurl / lightning address"))
	}
	firstURL, err := resolveFirstURL(in)
	if err != nil {
		return "", classifyErr(ClassOfferInvalid, err)
	}
	msats := amountSats * 1000
	meta, err := c.fetchPayMetadata(ctx, firstURL)
	if err != nil {
		return "", err
	}
	if strings.ToLower(meta.Tag) != "payrequest" {
		return "", classifyErr(ClassOfferInvalid, errors.New("not an lnurl-pay endpoint"))
	}
	if meta.Callback == "" {
		return "", classifyErr(ClassOfferInvalid, errors.New("missing callback in lnurlp response"))
	}
	if msats < meta.MinSendable || (meta.MaxSendable > 0 && msats > meta.MaxSendable) {
		return "", classifyErr(ClassOfferInvalid, fmt.Errorf("amount %d msats outside lnurlp bounds [%d,%d]", msats, meta.MinSendable, meta.MaxSendable))
	}
	cb, err := url.Parse(meta.Callback)
	if err != nil {
		return "", classifyErr(ClassOfferInvalid, err)
	}
	q := cb.Query()
	q.Set("amount", strconv.FormatInt(msats, 10))
	cb.RawQuery = q.Encode()
	inv, err := c.fetchInvoice(ctx, cb.String())
	if err != nil {
		return "", err
	}
	return inv, nil
}

type lnurlpMeta struct {
	Tag            string `json:"tag"`
	Callback       string `json:"callback"`
	MinSendable    int64  `json:"minSendable"`
	MaxSendable    int64  `json:"maxSendable"`
	Metadata       string `json:"metadata"`
	CommentAllowed int    `json:"commentAllowed"`
}

type lnurlpInvoiceResp struct {
	PR     string `json:"pr"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func resolveFirstURL(s string) (string, error) {
	if strings.HasPrefix(strings.ToLower(s), "http://") || strings.HasPrefix(strings.ToLower(s), "https://") {
		return s, nil
	}
	if strings.HasPrefix(strings.ToLower(s), "lnurlp://") {
		u := strings.TrimPrefix(s, "lnurlp://")
		return "https://" + u, nil
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || at == len(s)-1 {
		return "", errors.New("expected user@domain lightning address or https LNURL")
	}
	user := url.PathEscape(s[:at])
	host := strings.ToLower(strings.TrimSpace(s[at+1:]))
	if host == "" {
		return "", errors.New("invalid lightning address host")
	}
	return fmt.Sprintf("https://%s/.well-known/lnurlp/%s", host, user), nil
}

func (c *LNURLClient) fetchPayMetadata(ctx context.Context, u string) (lnurlpMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return lnurlpMeta{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return lnurlpMeta{}, classifyErr(ClassOfferUnreachable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return lnurlpMeta{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return lnurlpMeta{}, classifyErr(ClassOfferUnreachable, fmt.Errorf("lnurlp meta status %d", resp.StatusCode))
	}
	var meta lnurlpMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return lnurlpMeta{}, classifyErr(ClassOfferInvalid, fmt.Errorf("lnurlp json: %w", err))
	}
	return meta, nil
}

func (c *LNURLClient) fetchInvoice(ctx context.Context, callbackURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, callbackURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", classifyErr(ClassOfferUnreachable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", classifyErr(ClassOfferUnreachable, fmt.Errorf("lnurlp callback status %d", resp.StatusCode))
	}
	var out lnurlpInvoiceResp
	if err := json.Unmarshal(body, &out); err != nil {
		return "", classifyErr(ClassOfferInvalid, fmt.Errorf("lnurlp callback json: %w", err))
	}
	if strings.EqualFold(out.Status, "ERROR") {
		return "", classifyErr(ClassPaymentRejected, fmt.Errorf("lnurlp error: %s", out.Reason))
	}
	if strings.TrimSpace(out.PR) == "" {
		return "", classifyErr(ClassOfferInvalid, errors.New("empty pr in lnurlp callback"))
	}
	return strings.TrimSpace(out.PR), nil
}
