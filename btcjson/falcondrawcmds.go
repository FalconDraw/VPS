package btcjson

// GetDrawInfoCmd queries the current staking state for an upcoming or past draw.
type GetDrawInfoCmd struct {
	DrawHeight uint32
}

// NewGetDrawInfoCmd returns a new GetDrawInfoCmd.
func NewGetDrawInfoCmd(drawHeight uint32) *GetDrawInfoCmd {
	return &GetDrawInfoCmd{DrawHeight: drawHeight}
}

// GetDrawResultCmd queries the winner results of a settled Falcon Draw.
type GetDrawResultCmd struct {
	DrawHeight uint32
}

// NewGetDrawResultCmd returns a new GetDrawResultCmd.
func NewGetDrawResultCmd(drawHeight uint32) *GetDrawResultCmd {
	return &GetDrawResultCmd{DrawHeight: drawHeight}
}

// CreateStakingOutputCmd builds a hex-encoded staking pkScript.
// WalletHash is the 20-byte staker pubkey hash in hex (40 hex chars).
// DrawHeight must be a nonzero multiple of 144.
type CreateStakingOutputCmd struct {
	WalletHash string
	DrawHeight uint32
}

// NewCreateStakingOutputCmd returns a new CreateStakingOutputCmd.
func NewCreateStakingOutputCmd(walletHash string, drawHeight uint32) *CreateStakingOutputCmd {
	return &CreateStakingOutputCmd{
		WalletHash: walletHash,
		DrawHeight: drawHeight,
	}
}

// GetDrawSizeInfoCmd monitors the estimated settlement block size for the
// next draw as staking UTXOs accumulate. DrawHeight is optional; if omitted
// the next upcoming draw is used.
type GetDrawSizeInfoCmd struct {
	DrawHeight *uint32
}

// NewGetDrawSizeInfoCmd returns a new GetDrawSizeInfoCmd.
func NewGetDrawSizeInfoCmd(drawHeight *uint32) *GetDrawSizeInfoCmd {
	return &GetDrawSizeInfoCmd{DrawHeight: drawHeight}
}

// GetMempoolStakesCmd returns all unconfirmed staking transactions in the
// mempool for the given draw height.
type GetMempoolStakesCmd struct {
	DrawHeight uint32
}

// NewGetMempoolStakesCmd returns a new GetMempoolStakesCmd.
func NewGetMempoolStakesCmd(drawHeight uint32) *GetMempoolStakesCmd {
	return &GetMempoolStakesCmd{DrawHeight: drawHeight}
}

// GetDrawTicketsCmd returns the per-token bit scores for a specific wallet
// in a settled draw, enabling independent provability verification.
// WalletHash is the 40-hex-char staker pubkey hash.
// Count, if > 0, limits results to the top Count tickets by bits matched (desc).
type GetDrawTicketsCmd struct {
	WalletHash string
	DrawHeight uint32
	Count      *int
}

// NewGetDrawTicketsCmd returns a new GetDrawTicketsCmd.
func NewGetDrawTicketsCmd(walletHash string, drawHeight uint32) *GetDrawTicketsCmd {
	return &GetDrawTicketsCmd{WalletHash: walletHash, DrawHeight: drawHeight}
}

// GetDrawStakersCmd returns all wallets that staked in a draw, whether settled
// or upcoming. Works for settled draws by reading the settlement spend journal.
type GetDrawStakersCmd struct {
	DrawHeight uint32
}

// NewGetDrawStakersCmd returns a new GetDrawStakersCmd.
func NewGetDrawStakersCmd(drawHeight uint32) *GetDrawStakersCmd {
	return &GetDrawStakersCmd{DrawHeight: drawHeight}
}

// GetAddressUTXOsCmd returns all unspent outputs for an address in one call.
type GetAddressUTXOsCmd struct {
	Address string
}

// NewGetAddressUTXOsCmd returns a new GetAddressUTXOsCmd.
func NewGetAddressUTXOsCmd(address string) *GetAddressUTXOsCmd {
	return &GetAddressUTXOsCmd{Address: address}
}

// GetAddressBalanceCmd returns a spendable/staked summary for an address.
type GetAddressBalanceCmd struct {
	Address string
}

func NewGetAddressBalanceCmd(address string) *GetAddressBalanceCmd {
	return &GetAddressBalanceCmd{Address: address}
}

// GetWalletStakeHistoryCmd fetches all draws in which a wallet has staked,
// identified by its 20-byte wallet hash in hex.
type GetWalletStakeHistoryCmd struct {
	WalletHash string
}

func NewGetWalletStakeHistoryCmd(walletHash string) *GetWalletStakeHistoryCmd {
	return &GetWalletStakeHistoryCmd{WalletHash: walletHash}
}

// GetAddressIncomingCmd returns all confirmed incoming transfers to an address
// (outputs where address is NOT also an input — excludes change and self-spends).
type GetAddressIncomingCmd struct {
	Address string
}

func NewGetAddressIncomingCmd(address string) *GetAddressIncomingCmd {
	return &GetAddressIncomingCmd{Address: address}
}

// GetStakerStatsCmd returns cumulative staker participation counts: the most
// recently closed draw, the last 10 closed draws, and the all-time total.
type GetStakerStatsCmd struct{}

// NewGetStakerStatsCmd returns a new GetStakerStatsCmd.
func NewGetStakerStatsCmd() *GetStakerStatsCmd {
	return &GetStakerStatsCmd{}
}

func init() {
	flags := UsageFlag(0)
	MustRegisterCmd("getdrawinfo", (*GetDrawInfoCmd)(nil), flags)
	MustRegisterCmd("getdrawresult", (*GetDrawResultCmd)(nil), flags)
	MustRegisterCmd("createstakingoutput", (*CreateStakingOutputCmd)(nil), flags)
	MustRegisterCmd("getdrawsizeinfo", (*GetDrawSizeInfoCmd)(nil), flags)
	MustRegisterCmd("getmempoolstakes", (*GetMempoolStakesCmd)(nil), flags)
	MustRegisterCmd("getdrawtickets", (*GetDrawTicketsCmd)(nil), flags)
	MustRegisterCmd("getdrawstakers", (*GetDrawStakersCmd)(nil), flags)
	MustRegisterCmd("getaddressutxos", (*GetAddressUTXOsCmd)(nil), flags)
	MustRegisterCmd("getaddressbalance", (*GetAddressBalanceCmd)(nil), flags)
	MustRegisterCmd("getwalletstakehistory", (*GetWalletStakeHistoryCmd)(nil), flags)
	MustRegisterCmd("getaddressincoming", (*GetAddressIncomingCmd)(nil), flags)
	MustRegisterCmd("getstakerstats", (*GetStakerStatsCmd)(nil), flags)
}
