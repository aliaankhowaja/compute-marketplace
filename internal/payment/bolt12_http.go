package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPBolt12Fetcher calls an LNDK-compatible HTTP bridge (POST JSON, BOLT11 back).
type HTTPBolt12Fetcher struct {
	// EndpointURL is the full URL to POST (e.g. LNDK_HTTP_URL from env).
	EndpointURL string
	Client      *http.Client
}

type bolt12BridgeReq struct {
	Offer      string `json:"offer"`
	AmountSats int64  `json:"amount_sats"`
	PayerNote  string `json:"payer_note,omitempty"`
}

type bolt12BridgeResp struct {
	PaymentRequest string `json:"payment_request"`
	PR             string `json:"pr"`
	Error          string `json:"error"`
}

// FetchBolt12Invoice implements Bolt12Fetcher.
func (f *HTTPBolt12Fetcher) FetchBolt12Invoice(ctx context.Context, p Bolt12FetchParams) (string, error) {
	if f == nil || strings.TrimSpace(f.EndpointURL) == "" {
		return "", errBolt12FetchNotConfigured
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	body, err := json.Marshal(bolt12BridgeReq{
		Offer:      p.Offer,
		AmountSats: p.AmountSats,
		PayerNote:  p.PayerNote,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(f.EndpointURL), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", classifyErr(ClassOfferUnreachable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var br bolt12BridgeResp
	if err := json.Unmarshal(raw, &br); err == nil {
		if br.Error != "" {
			return "", classifyErr(ClassOfferInvalid, fmt.Errorf("bolt12 bridge: %s", br.Error))
		}
		if s := strings.TrimSpace(br.PaymentRequest); s != "" {
			return s, nil
		}
		if s := strings.TrimSpace(br.PR); s != "" {
			return s, nil
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", classifyErr(ClassOfferUnreachable, fmt.Errorf("bolt12 bridge status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	return "", classifyErr(ClassOfferInvalid, errors.New("bolt12 bridge: empty payment_request"))
}
