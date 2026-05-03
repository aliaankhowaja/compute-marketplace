package payment

import "time"

// BalanceInfo summarizes on-node liquidity (lnrpc aggregates).
type BalanceInfo struct {
	WalletConfirmedSats   int64
	WalletUnconfirmedSats int64
	ChannelLocalSats      int64 // sum of balances we can spend
	PendingOpenSats       int64
	PendingHTLCSats       int64 // sum of unresolved HTLCs (local unsettled balance)
	ActiveChannels        int32
	InactiveChannels      int32
}

// NodeHealth is a lightweight readiness snapshot for control-plane healthz.
type NodeHealth struct {
	LNDConnected  bool
	SyncedToChain bool
	SyncedToGraph bool
	BlockHeight   uint32
	Alias         string
	IdentityPub   string // hex
	LNDKEstimated bool // deprecated hint; always false when Bolt12 bridge removed
	IsTestNet     bool
}

// Bolt11Invoice is a minted LN invoice (typically BOLT11; may encode blinded path data).
type Bolt11Invoice struct {
	PaymentRequest string // encoded payment string for SendPayment / macaroon binding
	RHash          []byte // payment hash, 32 bytes
}

// PoolEntry holds a warm L402 preimage invoice from the invoice pool.
type PoolEntry struct {
	BOLT11      string
	PaymentHash []byte
	Preimage    []byte
	ExpiresAt   time.Time
}
