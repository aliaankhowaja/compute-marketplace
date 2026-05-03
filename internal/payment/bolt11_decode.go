package payment

import (
	"strings"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/zpay32"
)

// LooksLikeBolt11Invoice returns true when zpay32 can decode the string against net.
func LooksLikeBolt11Invoice(s string, net *chaincfg.Params) bool {
	s = strings.TrimSpace(s)
	if net == nil {
		net = &chaincfg.MainNetParams
	}
	_, err := zpay32.Decode(s, net)
	return err == nil
}

// DecodeBolt11 extracts payment-hash bytes from a BOLT11 payment request (duplicate detection).
func DecodeBolt11(s string, net *chaincfg.Params) ([]byte, error) {
	if net == nil {
		net = &chaincfg.MainNetParams
	}
	inv, err := zpay32.Decode(strings.TrimSpace(s), net)
	if err != nil {
		return nil, err
	}
	if inv.PaymentHash == nil {
		return nil, ErrMissingPaymentHash
	}
	ph := append([]byte(nil), inv.PaymentHash[:]...)
	return ph, nil
}
