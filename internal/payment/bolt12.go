package payment

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/zpay32"
)

// Bolt12Fetcher exchanges a validated BOLT12 offer string for an LND-payable invoice.
// Wire this to your LNDK/onion helper; it is deliberately narrow so production can swap implementations.
type Bolt12Fetcher interface {
	FetchBolt12Invoice(ctx context.Context, p Bolt12FetchParams) (paymentRequest string, err error)
}

// Bolt12FetcherFunc adapts a function to Bolt12Fetcher.
type Bolt12FetcherFunc func(ctx context.Context, p Bolt12FetchParams) (paymentRequest string, err error)

// FetchBolt12Invoice implements Bolt12Fetcher.
func (f Bolt12FetcherFunc) FetchBolt12Invoice(ctx context.Context, p Bolt12FetchParams) (string, error) {
	return f(ctx, p)
}

// Bolt12FetchParams carries payer metadata for auditing.
type Bolt12FetchParams struct {
	Offer      string
	AmountSats int64
	PayerNote  string
	Timeout    time.Duration
}

// ValidateOffer parses the human-readable Bolt 12 encoding and rejects obvious garbage locally.
func ValidateOffer(offer string) error {
	o := strings.TrimSpace(offer)
	if o == "" {
		return classifyErr(ClassOfferInvalid, errors.New("empty offer"))
	}
	hrp, _, err := bech32.DecodeNoLimit(strings.ToLower(o))
	if err != nil {
		return classifyErr(ClassOfferInvalid, err)
	}
	if !bolt12OfferHRP_ok(hrp) {
		return classifyErr(ClassOfferInvalid, errors.New("unsupported offer HRP"))
	}
	return nil
}

func bolt12OfferHRP_ok(hrp string) bool {
	// Bolt 12 offers reuse bech32m human-readable prefixes that start with lno*.
	// BOLT11 payment requests (`lnbc`/`lntb`/`ltc`) intentionally do not match.
	return strings.HasPrefix(hrp, "lno")
}

// LooksLikeBolt11Invoice returns true when zpay32 can decode the string against net.
func LooksLikeBolt11Invoice(s string, net *chaincfg.Params) bool {
	s = strings.TrimSpace(s)
	if net == nil {
		net = &chaincfg.MainNetParams
	}
	_, err := zpay32.Decode(s, net)
	return err == nil
}

// DecodeBolt11 extracts payment-hash bytes for duplicate detection.
func DecodeBolt11(s string, net *chaincfg.Params) ([]byte, error) {
	if net == nil {
		net = &chaincfg.MainNetParams
	}
	inv, err := zpay32.Decode(strings.TrimSpace(s), net)
	if err != nil {
		return nil, err
	}
	if inv.PaymentHash == nil {
		return nil, errors.New("invoice missing payment hash")
	}
	out := inv.PaymentHash[:]
	ph := append([]byte(nil), out[:]...)
	return ph, nil
}
