package payment

import (
	"errors"
	"fmt"

	"github.com/aliaankhowaja/sockt-backend/internal/payment/lnbot"
)

// Classification matches MVP payment error taxonomy.
type Classification string

const (
	ClassUnknown Classification = ""

	ClassRoutingFailure       Classification = "routing_failure"
	ClassOfferUnreachable     Classification = "offer_unreachable"
	ClassInvoiceExpired       Classification = "invoice_expired"
	ClassPaymentRejected      Classification = "payment_rejected"
	ClassOfferInvalid         Classification = "offer_invalid"
	ClassLightningUnavailable Classification = "lightning_unavailable"
	ClassManualTopUpRequired  Classification = "manual_topup_required"
	ClassAmountBelowMinimum   Classification = "amount_below_minimum"
)

// Classifier is attached to sentinel errors via errors.As.
type Classifier struct {
	Class Classification
	Err   error
}

func (c *Classifier) Error() string {
	if c.Err != nil {
		return fmt.Sprintf("%s: %v", c.Class, c.Err)
	}
	return string(c.Class)
}

func (c *Classifier) Unwrap() error { return c.Err }

func classifyErr(class Classification, err error) error {
	return &Classifier{Class: class, Err: err}
}

// Sentinel errors consumed by handlers.
var (
	ErrRoutingFailure       = classifyErr(ClassRoutingFailure, errors.New("routing failure"))
	ErrOfferUnreachable     = classifyErr(ClassOfferUnreachable, errors.New("offer unreachable"))
	ErrInvoiceExpired       = classifyErr(ClassInvoiceExpired, errors.New("invoice expired"))
	ErrPaymentRejected      = classifyErr(ClassPaymentRejected, errors.New("payment rejected"))
	ErrOfferInvalid         = classifyErr(ClassOfferInvalid, errors.New("offer invalid"))
	ErrLightningUnavailable = classifyErr(ClassLightningUnavailable, errors.New("lightning wallet unavailable"))
	ErrManualTopUpRequired  = classifyErr(ClassManualTopUpRequired, errors.New("manual top-up required"))
	ErrAmountBelowMinimum   = classifyErr(ClassAmountBelowMinimum, errors.New("amount below offer minimum"))

	// ErrMissingPaymentHash signals a decoded BOLT11 missing its payment_hash field.
	ErrMissingPaymentHash = errors.New("bolt11 invoice missing payment hash")
)

// Classify maps an error chain to Classification (unknown-safe).
func Classify(err error) Classification {
	var c *Classifier
	if errors.As(err, &c) {
		return c.Class
	}

	var he *lnbot.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.StatusCode == 429 || he.StatusCode >= 500:
			return ClassLightningUnavailable
		case he.StatusCode == 401 || he.StatusCode == 403:
			return ClassOfferInvalid
		case he.StatusCode >= 400:
			return ClassRoutingFailure
		}
	}

	return ClassUnknown
}
