package payment

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Classification matches MVP payment error taxonomy.
type Classification string

const (
	ClassUnknown Classification = ""

	ClassRoutingFailure      Classification = "routing_failure"
	ClassOfferUnreachable    Classification = "offer_unreachable"
	ClassInvoiceExpired      Classification = "invoice_expired"
	ClassPaymentRejected     Classification = "payment_rejected"
	ClassOfferInvalid        Classification = "offer_invalid"
	ClassLNDUnavailable      Classification = "lnd_unavailable"
	ClassManualTopUpRequired Classification = "manual_topup_required"
	ClassAmountBelowMinimum  Classification = "amount_below_minimum"
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
	ErrRoutingFailure      = classifyErr(ClassRoutingFailure, errors.New("routing failure"))
	ErrOfferUnreachable    = classifyErr(ClassOfferUnreachable, errors.New("offer unreachable"))
	ErrInvoiceExpired      = classifyErr(ClassInvoiceExpired, errors.New("invoice expired"))
	ErrPaymentRejected     = classifyErr(ClassPaymentRejected, errors.New("payment rejected"))
	ErrOfferInvalid        = classifyErr(ClassOfferInvalid, errors.New("offer invalid"))
	ErrLNDUnavailable      = classifyErr(ClassLNDUnavailable, errors.New("lnd unavailable"))
	ErrManualTopUpRequired = classifyErr(ClassManualTopUpRequired, errors.New("manual top-up required"))
	ErrAmountBelowMinimum  = classifyErr(ClassAmountBelowMinimum, errors.New("amount below offer minimum"))

	errBolt12FetchNotConfigured = classifyErr(ClassOfferUnreachable,
		errors.New("bolt12 invoice fetch not configured; wire payment.Bolt12Fetcher"))
)

// Classify maps an error chain to Classification (unknown-safe).
func Classify(err error) Classification {
	var c *Classifier
	if errors.As(err, &c) {
		return c.Class
	}

	st, ok := status.FromError(err)
	if ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded:
			return ClassLNDUnavailable
		case codes.Canceled:
			return ClassOfferUnreachable
		case codes.InvalidArgument:
			return ClassOfferInvalid
		}
		return ClassRoutingFailure
	}

	return ClassUnknown
}
