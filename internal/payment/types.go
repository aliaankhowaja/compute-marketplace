package payment

// BalanceInfo summarizes liquidity signals for dashboards (custodial wallet or legacy node fields).
type BalanceInfo struct {
	WalletConfirmedSats   int64
	WalletUnconfirmedSats int64
	ChannelLocalSats      int64 // unused for LNBOT; kept for API shape compatibility
	PendingOpenSats       int64
	PendingHTLCSats       int64
	ActiveChannels        int32
	InactiveChannels      int32
}

// NodeHealth is a lightweight readiness snapshot for optional probes.
type NodeHealth struct {
	LightningReachable bool
	SyncedToChain      bool
	SyncedToGraph      bool
	BlockHeight        uint32
	Alias              string
	IdentityPub        string
	IsTestNet          bool
}

// Bolt11Invoice is a minted LN invoice (BOLT11).
type Bolt11Invoice struct {
	PaymentRequest string // encoded payment string for payment + macaroon binding
	RHash          []byte // payment hash, 32 bytes
}
