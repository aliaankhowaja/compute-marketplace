package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/google/uuid"

	"github.com/aliaankhowaja/sockt-backend/internal/payment/lnbot"
)

const defaultPayWait = 25 * time.Second

// Payment is the façade consumed by billing and wallet HTTP handlers.
type Payment interface {
	PayInvoice(ctx context.Context, invoice string, idempotencyKey string) (preimageHex string, err error)
	CreateBOLT11Invoice(ctx context.Context, amountSats int64, memo string, expirySeconds int64) (*Bolt11Invoice, error)
	RequestDepositInvoice(ctx context.Context, agentID uuid.UUID, amountSats int64, expirySeconds int64, memo string, idempotencyKey string) (*DepositMintResult, error)
	RequestSandboxDepositInvoice(ctx context.Context, sandboxID uuid.UUID, amountSats int64, expirySeconds int64, memo string, idempotencyKey string) (*DepositMintResult, error)
	GetNodeBalance(ctx context.Context) (BalanceInfo, error)
	GetNodeHealth(ctx context.Context) (NodeHealth, error)
}

// Service implements Payment.
type Service struct {
	LNBOT             *lnbot.Client
	IDem              *IdempotencyCache
	PendingDeps       PendingDepositRecorder
	Chain             *chaincfg.Params
	PaymentSendExpiry time.Duration
}

// NewService constructs a Service with optional pending-deposit persistence.
func NewService(nb *lnbot.Client, idem *IdempotencyCache, pending PendingDepositRecorder) *Service {
	if idem == nil {
		idem = NewIdempotencyCache()
	}
	return &Service{
		LNBOT:       nb,
		IDem:        idem,
		PendingDeps: pending,
	}
}

// HasLightning reports whether outbound/inbound Lightning wallet operations are configured.
func (s *Service) HasLightning() bool {
	return s != nil && s.LNBOT != nil
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

func (s *Service) classifyBackend(err error) error {
	var he *lnbot.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.StatusCode == http.StatusTooManyRequests || he.StatusCode >= http.StatusInternalServerError:
			return fmt.Errorf("%w: %w", ErrLightningUnavailable, err)
		case he.StatusCode == http.StatusUnauthorized || he.StatusCode == http.StatusForbidden:
			return classifyErr(ClassOfferInvalid, err)
		}
		return classifyErr(ClassRoutingFailure, err)
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

// PayInvoice routes a payment_request through LNBOT with idempotent replay keyed by caller.
func (s *Service) PayInvoice(ctx context.Context, invoice string, idempotencyKey string) (string, error) {
	if s.LNBOT == nil {
		return "", classifyErr(ClassOfferInvalid, errors.New("lightning wallet not configured"))
	}
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

	ctx, cancel := context.WithTimeout(ctx, s.paymentTimeout())
	defer cancel()

	preHex, ferr := s.finalizePaymentRequest(ctx, invoice, strings.TrimSpace(idempotencyKey))
	if ferr != nil {
		s.IDem.SetFailure(phHex+"|"+idempotencyKey, ferr, defaultIdemTTL)
		s.IDem.SetFailure(idempotencyKey, ferr, defaultIdemTTL)
		return "", ferr
	}
	s.IDem.SetSuccess(phHex+"|"+idempotencyKey, preHex, defaultIdemTTL)
	s.IDem.SetSuccess(idempotencyKey, preHex, defaultIdemTTL)
	return preHex, nil
}

func (s *Service) finalizePaymentRequest(ctx context.Context, invoice string, idempotencyKey string) (string, error) {
	phBytes, err := s.decodePayHash(invoice)
	if err != nil {
		return "", err
	}
	preHex, payErr := s.LNBOT.SendPayment(ctx, strings.TrimSpace(invoice), idempotencyKey)
	if payErr != nil {
		return "", classifyForPay(payErr)
	}
	preImg, err := hex.DecodeString(strings.TrimSpace(preHex))
	if err != nil || len(preImg) != 32 {
		return "", classifyErr(ClassRoutingFailure, errors.New("missing preimage"))
	}
	if !VerifyPreimage(preImg, phBytes) {
		return "", classifyErr(ClassRoutingFailure, errors.New("preimage mismatch"))
	}
	return hex.EncodeToString(preImg), nil
}

func classifyForPay(payErr error) error {
	var cl *Classifier
	if errors.As(payErr, &cl) {
		return payErr
	}
	var pf *lnbot.PaymentFailedError
	if errors.As(payErr, &pf) {
		r := strings.ToLower(pf.FailureReason)
		switch {
		case strings.Contains(r, "timeout"):
			return classifyErr(ClassOfferUnreachable, payErr)
		case strings.Contains(r, "expired"), strings.Contains(r, "incorrect"):
			return classifyErr(ClassInvoiceExpired, payErr)
		case strings.Contains(r, "route"), strings.Contains(r, "balance"), strings.Contains(r, "liquidity"):
			return classifyErr(ClassRoutingFailure, payErr)
		case strings.Contains(r, "cancel"):
			return classifyErr(ClassPaymentRejected, payErr)
		default:
			return classifyErr(ClassRoutingFailure, payErr)
		}
	}
	return classifyErr(ClassRoutingFailure, payErr)
}

// CreateBOLT11Invoice backs prepaid deposit onboarding flows.
// expirySeconds is advisory only; LNBOT fixes invoice lifetime server-side (~60 minutes per API docs).
func (s *Service) CreateBOLT11Invoice(ctx context.Context, amountSats int64, memo string, expirySeconds int64) (*Bolt11Invoice, error) {
	if s.LNBOT == nil {
		return nil, classifyErr(ClassOfferInvalid, errors.New("lightning wallet not configured"))
	}
	if amountSats <= 0 {
		return nil, classifyErr(ClassOfferInvalid, errors.New("amount must be positive"))
	}
	if expirySeconds <= 0 {
		expirySeconds = 3600
	}
	bolt11, err := s.LNBOT.CreateInvoice(ctx, amountSats, memo, "")
	if err != nil {
		return nil, s.classifyBackend(err)
	}
	ph, err := DecodeBolt11(bolt11, s.chainParams())
	if err != nil {
		return nil, classifyErr(ClassOfferInvalid, err)
	}
	return &Bolt11Invoice{
		PaymentRequest: bolt11,
		RHash:          ph,
	}, nil
}

func (s *Service) GetNodeBalance(ctx context.Context) (BalanceInfo, error) {
	if s.LNBOT == nil {
		return BalanceInfo{}, classifyErr(ClassOfferInvalid, errors.New("lightning wallet not configured"))
	}
	w, err := s.LNBOT.GetWallet(ctx)
	if err != nil {
		return BalanceInfo{}, s.classifyBackend(err)
	}
	var out BalanceInfo
	out.WalletConfirmedSats = w.Available
	if out.WalletConfirmedSats <= 0 && w.Balance > 0 {
		out.WalletConfirmedSats = w.Balance
	}
	out.PendingHTLCSats = w.OnHold
	return out, nil
}

func (s *Service) GetNodeHealth(ctx context.Context) (NodeHealth, error) {
	var h NodeHealth
	if s.LNBOT == nil {
		return h, classifyErr(ClassOfferInvalid, errors.New("lightning wallet not configured"))
	}
	if _, err := s.LNBOT.Me(ctx); err != nil {
		return h, s.classifyBackend(err)
	}
	h.LightningReachable = true
	if w, err := s.LNBOT.GetWallet(ctx); err == nil && w != nil {
		h.Alias = w.WalletID
	}
	return h, nil
}
