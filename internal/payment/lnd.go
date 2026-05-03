package payment

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// LNDConfig holds bootstrap parameters for dialing a single LND node over gRPC.
type LNDConfig struct {
	// Address is host:port (default port 10009 when omitted).
	Address string

	// TLSCertPath is PEM for LND's TLS cert (or use TLSCertPEM).
	TLSCertPath string
	TLSCertPEM  []byte

	// MacaroonPath is the path to the macaroon file (or use MacaroonHex).
	MacaroonPath string
	MacaroonHex  string
}

type macaroonCred struct{ mac string }

func (m macaroonCred) RequireTransportSecurity() bool { return true }

func (m macaroonCred) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"macaroon": m.mac}, nil
}

// Client wraps lnrpc + router + invoices clients for the payment module.
type Client struct {
	LN       lnrpc.LightningClient
	Router   routerrpc.RouterClient
	Invoices invoicesrpc.InvoicesClient
	conn     *grpc.ClientConn
}

func normalizeAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.Contains(addr, ":") {
		return addr
	}
	return addr + ":10009"
}

func loadTLS(c LNDConfig) (credentials.TransportCredentials, error) {
	var pemBytes []byte
	var err error
	switch {
	case len(c.TLSCertPEM) > 0:
		pemBytes = c.TLSCertPEM
	case c.TLSCertPath != "":
		pemBytes, err = os.ReadFile(c.TLSCertPath)
		if err != nil {
			return nil, fmt.Errorf("read tls cert: %w", err)
		}
	default:
		return nil, errors.New("lnd tls: TLSCertPath or TLSCertPEM required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("lnd tls: failed to append PEM certs")
	}
	return credentials.NewClientTLSFromCert(pool, ""), nil
}

func loadMacHex(c LNDConfig) (string, error) {
	switch {
	case c.MacaroonHex != "":
		return strings.TrimSpace(strings.TrimPrefix(c.MacaroonHex, "0x")), nil
	case c.MacaroonPath != "":
		raw, err := os.ReadFile(c.MacaroonPath)
		if err != nil {
			return "", fmt.Errorf("read macaroon: %w", err)
		}
		txt := strings.TrimSpace(string(raw))
		if _, err := hex.DecodeString(txt); err == nil && len(txt) >= 64 && len(txt)%2 == 0 {
			return txt, nil
		}
		return hex.EncodeToString([]byte(strings.TrimSpace(string(raw)))), nil
	default:
		return "", errors.New("lnd macaroon: MacaroonPath or MacaroonHex required")
	}
}

// DialLND opens a long-lived gRPC connection to LND.
func DialLND(ctx context.Context, c LNDConfig) (*Client, error) {
	addr := normalizeAddress(c.Address)
	if addr == "" {
		return nil, errors.New("lnd: Address required")
	}
	tlsCreds, err := loadTLS(c)
	if err != nil {
		return nil, err
	}
	macHex, err := loadMacHex(c)
	if err != nil {
		return nil, err
	}

	kp := keepalive.ClientParameters{
		Time:                15 * time.Second,
		Timeout:             5 * time.Second,
		PermitWithoutStream: true,
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithPerRPCCredentials(macaroonCred{mac: macHex}),
		grpc.WithKeepaliveParams(kp),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16 << 20)),
	}

	conn, err := grpc.DialContext(ctx, addr, opts...)
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:     conn,
		LN:       lnrpc.NewLightningClient(conn),
		Router:   routerrpc.NewRouterClient(conn),
		Invoices: invoicesrpc.NewInvoicesClient(conn),
	}, nil
}

// Close shuts down the gRPC connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func amountSat(a *lnrpc.Amount) int64 {
	if a == nil {
		return 0
	}
	return int64(a.GetSat())
}

func (c *Client) channelCounts(ctx context.Context) (active, inactive int32, _ error) {
	resp, err := c.LN.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		return 0, 0, err
	}
	var a, ia int32
	for _, ch := range resp.Channels {
		if ch.Active {
			a++
		} else {
			ia++
		}
	}
	return a, ia, nil
}

// GetNodeBalance aggregates liquidity used by dashboards/health.
func (c *Client) GetNodeBalance(ctx context.Context) (BalanceInfo, error) {
	var out BalanceInfo
	wb, err := c.LN.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
	if err != nil {
		return out, err
	}
	cb, err := c.LN.ChannelBalance(ctx, &lnrpc.ChannelBalanceRequest{})
	if err != nil {
		return out, err
	}
	a, inactive, err := c.channelCounts(ctx)
	if err != nil {
		return out, err
	}
	out.WalletConfirmedSats = wb.GetConfirmedBalance()
	out.WalletUnconfirmedSats = wb.GetUnconfirmedBalance()
	out.ChannelLocalSats = amountSat(cb.LocalBalance)
	out.PendingOpenSats = amountSat(cb.PendingOpenLocalBalance)
	out.PendingHTLCSats = amountSat(cb.UnsettledLocalBalance)
	out.ActiveChannels = a
	out.InactiveChannels = inactive
	return out, nil
}

// GetNodeHealth returns GetInfo-derived signals for MVP /healthz.
func (c *Client) GetNodeHealth(ctx context.Context) (NodeHealth, error) {
	info, err := c.LN.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	var h NodeHealth
	if err != nil {
		return h, err
	}
	h.LNDConnected = true
	h.SyncedToChain = info.SyncedToChain
	h.SyncedToGraph = info.SyncedToGraph
	h.BlockHeight = info.BlockHeight
	h.Alias = info.Alias
	h.IdentityPub = info.IdentityPubkey
	if len(info.Chains) > 0 && info.Chains[0].GetNetwork() == "mainnet" {
		h.IsTestNet = false
	} else {
		h.IsTestNet = true
	}
	return h, nil
}

// AddMintedInvoice calls AddInvoice including optional RPreimage hints.
func (c *Client) AddMintedInvoice(ctx context.Context, inv *lnrpc.Invoice) (*lnrpc.AddInvoiceResponse, error) {
	return c.LN.AddInvoice(ctx, inv)
}

// LookupInvoice fetches invoice status by payment hash for deposit reconciliation.
func (c *Client) LookupInvoice(ctx context.Context, paymentHash []byte) (*lnrpc.Invoice, error) {
	return c.LN.LookupInvoice(ctx, &lnrpc.PaymentHash{RHash: paymentHash})
}

// CancelInvoice removes an unpaid invoice from local state best-effort.
func (c *Client) CancelInvoice(ctx context.Context, paymentHash []byte) error {
	if len(paymentHash) != 32 {
		return ErrOfferInvalid
	}
	_, err := c.Invoices.CancelInvoice(ctx, &invoicesrpc.CancelInvoiceMsg{
		PaymentHash: paymentHash,
	})
	return err
}

// SendPaymentViaRouter pays encoded payment requests (BOLT11 or blinded where LND supports it).
func (c *Client) SendPaymentViaRouter(ctx context.Context, paymentRequest string, timeout time.Duration) (*lnrpc.Payment, error) {
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest:    paymentRequest,
		TimeoutSeconds:    int32(timeout.Seconds()),
		FeeLimitMsat:      math.MaxInt64,
		MaxParts:          10,
		NoInflightUpdates: true,
	}
	stream, err := c.Router.SendPaymentV2(ctx, req)
	if err != nil {
		return nil, err
	}
	var last *lnrpc.Payment
	for {
		upd, err := stream.Recv()
		if err != nil {
			return last, classifyPaymentTerminal(last, err)
		}
		last = upd
		switch upd.GetStatus() {
		case lnrpc.Payment_SUCCEEDED:
			return upd, nil
		case lnrpc.Payment_FAILED:
			return upd, classifyPaymentFailure(upd)
		}
	}
}

func classifyPaymentTerminal(last *lnrpc.Payment, streamErr error) error {
	if streamErr != nil {
		return streamErr
	}
	if last == nil {
		return ErrRoutingFailure
	}
	switch last.Status {
	case lnrpc.Payment_SUCCEEDED:
		return nil
	case lnrpc.Payment_FAILED:
		return classifyPaymentFailure(last)
	default:
		return ErrOfferUnreachable
	}
}

func classifyPaymentFailure(p *lnrpc.Payment) error {
	if p == nil {
		return ErrRoutingFailure
	}
	switch p.FailureReason {
	case lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS:
		return fmt.Errorf("%w: incorrect payment details (expiry?)", ErrInvoiceExpired)
	case lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE,
		lnrpc.PaymentFailureReason_FAILURE_REASON_INSUFFICIENT_BALANCE:
		return fmt.Errorf("%w: %v", ErrRoutingFailure, p.FailureReason)
	case lnrpc.PaymentFailureReason_FAILURE_REASON_TIMEOUT:
		return fmt.Errorf("%w: payment timeout", ErrOfferUnreachable)
	case lnrpc.PaymentFailureReason_FAILURE_REASON_CANCELED:
		return fmt.Errorf("%w: canceled", ErrPaymentRejected)
	default:
		return fmt.Errorf("%w: %v", ErrRoutingFailure, p.FailureReason)
	}
}
