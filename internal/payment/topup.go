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
	offerType, offer, err := s.Agents.Lookup(ctx, agentID)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(offerType, "manual_bolt11") {
		s.IDem.SetFailure(idempotencyKey, ErrManualTopUpRequired, defaultIdemTTL)
		return "", ErrManualTopUpRequired
	}
	o := strings.TrimSpace(offer)
	if o == "" {
		classed := classifyErr(ClassOfferInvalid, errors.New("missing bolt12 offer"))
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
