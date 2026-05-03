package payment

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// TopUpBalance orchestrates Bolt12 invoice_request → invoice → settle for billing pulls.
func (s *Service) TopUpBalance(ctx context.Context, agentID uuid.UUID, sats int64, idempotencyKey string) (string, error) {
	if replay, ok := s.IDem.Get(idempotencyKey); ok && replay != nil && replay.err == nil && replay.preimageHex != "" {
		return replay.preimageHex, nil
	}
	if replay, ok := s.IDem.Get(idempotencyKey); ok && replay != nil && replay.err != nil {
		return "", replay.err
	}
	if s.Agents == nil {
		return "", classifyErr(ClassOfferInvalid, errors.New("agents store not wired"))
	}
	info, err := s.Agents.Lookup(ctx, agentID)
	if err != nil {
		return "", err
	}
	offerType := strings.ToLower(strings.TrimSpace(info.OfferType))
	switch offerType {
	case "manual_bolt11":
		s.IDem.SetFailure(idempotencyKey, ErrManualTopUpRequired, defaultIdemTTL)
		return "", ErrManualTopUpRequired
	case "lnurl_pay":
		addr := strings.TrimSpace(info.LightningAddress)
		if addr == "" {
			classed := classifyErr(ClassOfferInvalid, errors.New("missing lightning_address for lnurl_pay"))
			s.IDem.SetFailure(idempotencyKey, classed, defaultIdemTTL)
			return "", classed
		}
		if s.LNURL == nil {
			err := classifyErr(ClassOfferInvalid, errors.New("lnurl client not configured"))
			s.IDem.SetFailure(idempotencyKey, err, defaultIdemTTL)
			return "", err
		}
		inv, err := s.LNURL.FetchInvoice(ctx, addr, sats)
		if err != nil {
			s.IDem.SetFailure(idempotencyKey, err, defaultIdemTTL)
			return "", err
		}
		preImgHex, err := s.finalizePaymentRequest(ctx, inv)
		if err != nil {
			s.IDem.SetFailure(idempotencyKey, err, defaultIdemTTL)
			return "", err
		}
		s.IDem.SetSuccess(idempotencyKey, preImgHex, defaultIdemTTL)
		return preImgHex, nil
	case "bolt12", "":
		// Primary BOLT12 path (empty offer_type treated as bolt12 for backward compatibility).
	default:
		classed := classifyErr(ClassOfferInvalid, fmt.Errorf("unsupported offer_type %q", offerType))
		s.IDem.SetFailure(idempotencyKey, classed, defaultIdemTTL)
		return "", classed
	}

	o := strings.TrimSpace(info.Bolt12Offer)
	if o == "" {
		classed := classifyErr(ClassOfferInvalid, errors.New("missing bolt12 offer (set offer_type=lnurl_pay or register an offer)"))
		s.IDem.SetFailure(idempotencyKey, classed, defaultIdemTTL)
		return "", classed
	}
	note := fmt.Sprintf("topup:%s:%s", agentID.String(), idempotencyKey)
	reqStr, err := s.RequestBOLT12Invoice(ctx, o, sats, note)
	if err != nil {
		s.IDem.SetFailure(idempotencyKey, err, defaultIdemTTL)
		return "", err
	}
	preImgHex, err := s.finalizePaymentRequest(ctx, reqStr)
	if err != nil {
		s.IDem.SetFailure(idempotencyKey, err, defaultIdemTTL)
		return "", err
	}
	s.IDem.SetSuccess(idempotencyKey, preImgHex, defaultIdemTTL)
	return preImgHex, nil
}

func (s *Service) finalizePaymentRequest(ctx context.Context, invoice string) (string, error) {
	phBytes, err := s.decodePayHash(invoice)
	if err != nil {
		return "", err
	}
	payment, payErr := s.LND.SendPaymentViaRouter(ctx, strings.TrimSpace(invoice), s.paymentTimeout())
	if payErr != nil {
		return "", classifyForPay(payment, payErr)
	}
	preImg, err := hex.DecodeString(strings.TrimSpace(payment.PaymentPreimage))
	if err != nil || len(preImg) != 32 {
		return "", classifyErr(ClassRoutingFailure, errors.New("missing preimage"))
	}
	if !VerifyPreimage(preImg, phBytes) {
		return "", classifyErr(ClassRoutingFailure, errors.New("preimage mismatch"))
	}
	return hex.EncodeToString(preImg), nil
}
