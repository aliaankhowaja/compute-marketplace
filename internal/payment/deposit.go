package payment

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// PendingDepositRecorder persists Bolt11 invoices awaiting inbound settlement (custodial wallet via LNBOT).
type PendingDepositRecorder interface {
	InsertPendingDeposit(ctx context.Context, agentID uuid.UUID, payHash []byte, bolt11 string, amountMsats int64) error
	InsertPendingSandboxDeposit(ctx context.Context, sandboxID uuid.UUID, payHash []byte, bolt11 string, amountMsats int64) error
}

// DepositMintResult is one inbound prepaid deposit invoice for an agent.
type DepositMintResult struct {
	PaymentRequest string // BOLT11
	RHash          []byte // payment hash bytes (32)
	AmountMsats    int64
	ExpirySeconds  int64
}

type depositIdemSnap struct {
	PR          string `json:"pr"`
	RHashHex    string `json:"rh"`
	AmountMsats int64  `json:"ms"`
	ExpSecs     int64  `json:"exp"`
}

const depositIdemPrefix = "depmint:"

// RequestDepositInvoice mints a BOLT11 payable to the Sockt treasury wallet and records a pending row until settled.
func (s *Service) RequestDepositInvoice(ctx context.Context, agentID uuid.UUID, amountSats int64, expirySeconds int64, memo string, idempotencyKey string) (*DepositMintResult, error) {
	if s == nil || s.LNBOT == nil {
		return nil, classifyErr(ClassOfferInvalid, errors.New("lightning wallet not configured"))
	}
	if s.PendingDeps == nil {
		return nil, classifyErr(ClassOfferInvalid, errors.New("deposit recorder not wired"))
	}
	key := strings.TrimSpace(idempotencyKey)
	if key != "" && s.IDem != nil {
		if replay, ok := s.IDem.Get(depositIdemPrefix + key); ok && replay != nil && replay.err != nil {
			return nil, replay.err
		}
		if replay, ok := s.IDem.Get(depositIdemPrefix + key); ok && replay != nil && replay.err == nil && replay.preimageHex != "" {
			var snap depositIdemSnap
			if json.Unmarshal([]byte(replay.preimageHex), &snap) == nil && snap.PR != "" && len(snap.RHashHex) == 64 {
				rh, err := hex.DecodeString(snap.RHashHex)
				if err == nil && len(rh) == 32 {
					return &DepositMintResult{
						PaymentRequest: snap.PR,
						RHash:          rh,
						AmountMsats:    snap.AmountMsats,
						ExpirySeconds:  snap.ExpSecs,
					}, nil
				}
			}
		}
	}
	if amountSats <= 0 {
		amountSats = 50_000
	}
	if expirySeconds <= 0 {
		expirySeconds = 3600
	}
	if memo == "" {
		memo = fmt.Sprintf("deposit:%s", agentID)
	}
	inv, err := s.CreateBOLT11Invoice(ctx, amountSats, memo, expirySeconds)
	if err != nil {
		if key != "" && s.IDem != nil {
			s.IDem.SetFailure(depositIdemPrefix+key, err, defaultIdemTTL)
		}
		return nil, err
	}
	ph := inv.RHash
	if len(ph) != 32 {
		return nil, classifyErr(ClassOfferInvalid, errors.New("invoice_bad_hash"))
	}
	msats := amountSats * 1000
	if err := s.PendingDeps.InsertPendingDeposit(ctx, agentID, ph, inv.PaymentRequest, msats); err != nil {
		return nil, err
	}
	out := &DepositMintResult{
		PaymentRequest: inv.PaymentRequest,
		RHash:          ph,
		AmountMsats:    msats,
		ExpirySeconds:  expirySeconds,
	}
	if key != "" && s.IDem != nil {
		snapJSON, err := json.Marshal(depositIdemSnap{
			PR:          out.PaymentRequest,
			RHashHex:    hex.EncodeToString(out.RHash),
			AmountMsats: out.AmountMsats,
			ExpSecs:     out.ExpirySeconds,
		})
		if err == nil {
			s.IDem.SetSuccess(depositIdemPrefix+key, string(snapJSON), defaultIdemTTL)
		}
	}
	return out, nil
}

const sandboxDepositIdemPrefix = "sbxdepmint:"

// RequestSandboxDepositInvoice mints a BOLT11 sized for a sandbox prepay/topup chunk and records a
// pending row keyed by sandbox_id (no agent involved). Idempotent on idempotencyKey.
func (s *Service) RequestSandboxDepositInvoice(ctx context.Context, sandboxID uuid.UUID, amountSats int64, expirySeconds int64, memo string, idempotencyKey string) (*DepositMintResult, error) {
	if s == nil || s.LNBOT == nil {
		return nil, classifyErr(ClassOfferInvalid, errors.New("lightning wallet not configured"))
	}
	if s.PendingDeps == nil {
		return nil, classifyErr(ClassOfferInvalid, errors.New("deposit recorder not wired"))
	}
	key := strings.TrimSpace(idempotencyKey)
	if key != "" && s.IDem != nil {
		if replay, ok := s.IDem.Get(sandboxDepositIdemPrefix + key); ok && replay != nil && replay.err != nil {
			return nil, replay.err
		}
		if replay, ok := s.IDem.Get(sandboxDepositIdemPrefix + key); ok && replay != nil && replay.err == nil && replay.preimageHex != "" {
			var snap depositIdemSnap
			if json.Unmarshal([]byte(replay.preimageHex), &snap) == nil && snap.PR != "" && len(snap.RHashHex) == 64 {
				rh, err := hex.DecodeString(snap.RHashHex)
				if err == nil && len(rh) == 32 {
					return &DepositMintResult{
						PaymentRequest: snap.PR,
						RHash:          rh,
						AmountMsats:    snap.AmountMsats,
						ExpirySeconds:  snap.ExpSecs,
					}, nil
				}
			}
		}
	}
	if amountSats <= 0 {
		amountSats = 30
	}
	if expirySeconds <= 0 {
		expirySeconds = 3600
	}
	if memo == "" {
		memo = fmt.Sprintf("sandbox-prepay:%s", sandboxID)
	}
	inv, err := s.CreateBOLT11Invoice(ctx, amountSats, memo, expirySeconds)
	if err != nil {
		if key != "" && s.IDem != nil {
			s.IDem.SetFailure(sandboxDepositIdemPrefix+key, err, defaultIdemTTL)
		}
		return nil, err
	}
	ph := inv.RHash
	if len(ph) != 32 {
		return nil, classifyErr(ClassOfferInvalid, errors.New("invoice_bad_hash"))
	}
	msats := amountSats * 1000
	if err := s.PendingDeps.InsertPendingSandboxDeposit(ctx, sandboxID, ph, inv.PaymentRequest, msats); err != nil {
		return nil, err
	}
	out := &DepositMintResult{
		PaymentRequest: inv.PaymentRequest,
		RHash:          ph,
		AmountMsats:    msats,
		ExpirySeconds:  expirySeconds,
	}
	if key != "" && s.IDem != nil {
		snapJSON, err := json.Marshal(depositIdemSnap{
			PR:          out.PaymentRequest,
			RHashHex:    hex.EncodeToString(out.RHash),
			AmountMsats: out.AmountMsats,
			ExpSecs:     out.ExpirySeconds,
		})
		if err == nil {
			s.IDem.SetSuccess(sandboxDepositIdemPrefix+key, string(snapJSON), defaultIdemTTL)
		}
	}
	return out, nil
}
