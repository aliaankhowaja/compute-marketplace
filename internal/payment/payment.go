package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/google/uuid"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultPayWait = 25 * time.Second

// Payment is the façade consumed by billing, L402, and wallet HTTP handlers.
type Payment interface {
	PayInvoice(ctx context.Context, invoice string, idempotencyKey string) (preimageHex string, err error)
	CreateBOLT11Invoice(ctx context.Context, amountSats int64, memo string, expirySeconds int64) (*Bolt11Invoice, error)
	RequestDepositInvoice(ctx context.Context, agentID uuid.UUID, amountSats int64, expirySeconds int64, memo string, idempotencyKey string) (*DepositMintResult, error)
	MintL402Invoice(ctx context.Context) (*Bolt11Invoice, []byte, error)
	GetNodeBalance(ctx context.Context) (BalanceInfo, error)
	GetNodeHealth(ctx context.Context) (NodeHealth, error)
}

// Service implements Payment.
type Service struct {
	LND               *Client
	Pool              *InvoicePool
	IDem              *IdempotencyCache
	PendingDeps       PendingDepositRecorder
	Chain             *chaincfg.Params
	PaymentSendExpiry time.Duration
}

// NewService constructs a Service with optional pool (L402) and pending-deposit persistence.
func NewService(lnd *Client, pool *InvoicePool, idem *IdempotencyCache, pending PendingDepositRecorder) *Service {
	if idem == nil {
		idem = NewIdempotencyCache()
	}
	return &Service{
		LND:         lnd,
		Pool:        pool,
		IDem:        idem,
		PendingDeps: pending,
	}
}

// Compile-time assertion.
var _ Payment = (*Service)(nil)

// VerifyPreimage matches preimage bytes to SHA256 preimage hash (L402).
func VerifyPreimage(preimage, paymentHash []byte) bool {
	if len(preimage) != 32 || len(paymentHash) != 32 {
		return false
	}
	sum := sha256.Sum256(preimage)
	return subtleConstantTimeCompare(sum[:], paymentHash)
}

func subtleConstantTimeCompare(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (s *Service) chainParams() *chaincfg.Params {
	if s.Chain != nil {
		return s.Chain
	}
	return &chaincfg.MainNetParams
}

func (s *Service) paymentTimeout() time.Duration {
	if s.PaymentSendExpiry > 0 {
		return s.PaymentSendExpiry
	}
	return defaultPayWait
}

func (s *Service) classifyRPC(err error) error {
	st, ok := status.FromError(err)
	if ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded:
			return fmt.Errorf("%w: %w", ErrLNDUnavailable, err)
		}
	}
	return err
}

func (s *Service) decodePayHash(invoice string) ([]byte, error) {
	ph, err := DecodeBolt11(invoice, s.chainParams())
	if err != nil {
		return nil, classifyErr(ClassOfferInvalid, fmt.Errorf("decode payment_request: %w", err))
	}
	return ph, nil
}

// PayInvoice routes a payment_request through LND with idempotent replay keyed by caller.
func (s *Service) PayInvoice(ctx context.Context, invoice string, idempotencyKey string) (string, error) {
	if replay, ok := s.IDem.Get(idempotencyKey); ok && replay != nil && replay.err == nil && replay.preimageHex != "" {
		return replay.preimageHex, nil
	}
	if replay, ok := s.IDem.Get(idempotencyKey); ok && replay != nil && replay.err != nil {
		return "", replay.err
	}
	phPreview, decErr := s.decodePayHash(invoice)
	if decErr != nil {
		s.IDem.SetFailure(idempotencyKey, decErr, defaultIdemTTL)
		return "", decErr
	}
	phHex := hex.EncodeToString(phPreview)

	preHex, ferr := s.finalizePaymentRequest(ctx, invoice)
	if ferr != nil {
		s.IDem.SetFailure(phHex+"|"+idempotencyKey, ferr, defaultIdemTTL)
		s.IDem.SetFailure(idempotencyKey, ferr, defaultIdemTTL)
		return "", ferr
	}
	s.IDem.SetSuccess(phHex+"|"+idempotencyKey, preHex, defaultIdemTTL)
	s.IDem.SetSuccess(idempotencyKey, preHex, defaultIdemTTL)
	return preHex, nil
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

func classifyForPay(p *lnrpc.Payment, payErr error) error {
	var cl *Classifier
	if errors.As(payErr, &cl) {
		return payErr
	}
	if p != nil {
		return classifyPaymentFailure(p)
	}
	return classifyErr(ClassRoutingFailure, payErr)
}

// CreateBOLT11Invoice backs prepaid deposit onboarding flows.
func (s *Service) CreateBOLT11Invoice(ctx context.Context, amountSats int64, memo string, expirySeconds int64) (*Bolt11Invoice, error) {
	if amountSats <= 0 {
		return nil, classifyErr(ClassOfferInvalid, errors.New("amount must be positive"))
	}
	if expirySeconds <= 0 {
		expirySeconds = 3600
	}
	resp, err := s.LND.AddMintedInvoice(ctx, &lnrpc.Invoice{
		Value:   amountSats,
		Memo:    memo,
		Expiry:  expirySeconds,
		Private: true,
	})
	if err != nil {
		return nil, s.classifyRPC(err)
	}
	return &Bolt11Invoice{
		PaymentRequest: resp.GetPaymentRequest(),
		RHash:          resp.GetRHash(),
	}, nil
}

// MintL402Invoice pops a warm preimage invoice or performs a synchronous AddInvoice fallback.
func (s *Service) MintL402Invoice(ctx context.Context) (*Bolt11Invoice, []byte, error) {
	if s.Pool != nil {
		if ent, ok := s.Pool.Pop(ctx); ok && ent != nil {
			return &Bolt11Invoice{
				PaymentRequest: ent.BOLT11,
				RHash:          ent.PaymentHash,
			}, ent.Preimage, nil
		}
	}
	preimage := make([]byte, 32)
	if _, err := rand.Read(preimage); err != nil {
		return nil, nil, err
	}
	resp, err := s.LND.AddMintedInvoice(ctx, &lnrpc.Invoice{
		RPreimage: preimage,
		ValueMsat: l402SatMsat,
		Memo:      l402PoolMemo,
		Expiry:    int64(defaultPoolInvoiceTTL / time.Second),
	})
	if err != nil {
		return nil, nil, s.classifyRPC(err)
	}
	return &Bolt11Invoice{
			PaymentRequest: resp.GetPaymentRequest(),
			RHash:          resp.GetRHash(),
		},
		preimage,
		nil
}

func (s *Service) GetNodeBalance(ctx context.Context) (BalanceInfo, error) {
	return s.LND.GetNodeBalance(ctx)
}

func (s *Service) GetNodeHealth(ctx context.Context) (NodeHealth, error) {
	h, err := s.LND.GetNodeHealth(ctx)
	if err != nil {
		return h, s.classifyRPC(err)
	}
	h.LNDKEstimated = false
	return h, nil
}
