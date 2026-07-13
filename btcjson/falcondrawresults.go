package btcjson

// DrawStakerInfo describes a single staker's position for an upcoming draw.
type DrawStakerInfo struct {
	WalletHash string  `json:"wallethash"`
	Tokens     int64   `json:"tokens"`
	FDRW       float64 `json:"fdrw"`
}

// GetDrawInfoResult is returned by the getdrawinfo command.
type GetDrawInfoResult struct {
	DrawHeight        uint32           `json:"drawheight"`
	DrawSequence      int              `json:"drawsequence"`     // 1-based count of real draws including this one
	SettlementHeight  uint32           `json:"settlementheight"` // 0 until VDF settles (no longer always drawHeight+1)
	Settled           bool             `json:"settled"`
	BlocksUntilDraw   int32            `json:"blocksuntildraw"`   // blocks until staking window closes (0 when locked/settled)
	WinProbabilityPct float64          `json:"winprobabilitypct"` // always 10.0 — WinMultiplier=10
	TotalPoolFDRW     float64          `json:"totalpool"`
	RealPoolFDRW      float64          `json:"realpool"`
	JackpotPoolFDRW   float64          `json:"jackpotpool"`
	NumStakers        int              `json:"numstakers"`
	TotalTokens       int64            `json:"totaltokens"`
	Stakers           []DrawStakerInfo `json:"stakers"`
	// VDF status fields
	VDFComputing        bool   `json:"vdfcomputing"`        // true while Sloth eval in progress
	VDFProgressPct      uint32 `json:"vdfprogresspct"`      // real 10%-increment progress (0,10,...,90); 100 once done
	VDFSecondsRemaining int64  `json:"vdfsecondsremaining"` // estimated seconds until VDF done
	VDFElapsedSecs      int64  `json:"vdfelapsedsecs"`      // seconds elapsed since VDF started
	VDFActualSecs       int64  `json:"vdfactualsecs"`       // actual seconds taken (once complete)
	VDFDone             bool   `json:"vdfdone"`             // true once VDF result is available
	VDFT                uint64 `json:"vdft,omitempty"`      // T steps used (settled draws)
	VDFOutput           string `json:"vdfoutput,omitempty"` // hex-encoded output (settled draws)
	CurrentVDFT         uint64 `json:"currentvdft"`         // T currently in use (or target for next draw)
}

// DrawWinnerInfo describes the prize winner in a settled draw.
type DrawWinnerInfo struct {
	WalletHash string  `json:"wallethash"`
	PrizeFDRW  float64 `json:"prize"`
}

// GetDrawSizeInfoResult is returned by the getdrawsizeinfo command.
type GetDrawSizeInfoResult struct {
	DrawHeight          uint32  `json:"drawheight"`
	BlocksUntilDraw     int32   `json:"blocksuntildraw"`
	NumStakingUTXOs     int     `json:"numstakingutxos"`
	NumWallets          int     `json:"numwallets"`
	EstSettlementBytes  int64   `json:"estsettlementbytes"`
	EstSettlementWeight int64   `json:"estsettlementweight"`
	MaxSettlementWeight int64   `json:"maxsettlementweight"`
	WeightUsedPct       float64 `json:"weightusedpct"`
}

// MempoolStakeEntry describes a single unconfirmed staking output in the mempool.
type MempoolStakeEntry struct {
	TxID       string  `json:"txid"`
	Vout       uint32  `json:"vout"`
	WalletHash string  `json:"wallethash"`
	FDRW       float64 `json:"fdrw"`
}

// GetMempoolStakesResult is returned by the getmempoolstakes command.
type GetMempoolStakesResult struct {
	DrawHeight uint32              `json:"drawheight"`
	Stakes     []MempoolStakeEntry `json:"stakes"`
}

// GetDrawResultResult is returned by the getdrawresult command.
type GetDrawResultResult struct {
	DrawHeight       uint32           `json:"drawheight"`
	DrawSequence     int              `json:"drawsequence"` // 1-based count up to and including this draw
	DrawBlockHash    string           `json:"drawnblockhash"`
	TotalPoolFDRW    float64          `json:"totalpool"`
	Settled          bool             `json:"settled"` // false while awaiting VDF result
	JackpotWon       bool             `json:"jackpotwon"`
	RolloverFDRW     float64          `json:"rollover"`
	WinningPos       int64            `json:"winningpos"`  // token position selected by VDF-derived seed
	TotalTokens      int64            `json:"totaltokens"` // total real-staker tokens in the draw
	NumWinners       int              `json:"numwinners"`
	Winners          []DrawWinnerInfo `json:"winners"`
	SettlementTxID   string           `json:"settlementtxid"`
	SettlementHeight int32            `json:"settlementheight"` // block containing VDF+settlement
	VDFT             uint64           `json:"vdft"`             // Sloth steps computed
	VDFOutput        string           `json:"vdfoutput"`        // hex-encoded 256-byte output
}

// DrawStakerEntry describes one wallet's stake position in a draw.
type DrawStakerEntry struct {
	WalletHash string  `json:"wallethash"`
	FDRW       float64 `json:"fdrw"`
	Tokens     int64   `json:"tokens"`
}

// GetDrawStakersResult is returned by the getdrawstakers command.
type GetDrawStakersResult struct {
	DrawHeight uint32            `json:"drawheight"`
	Settled    bool              `json:"settled"`
	Stakers    []DrawStakerEntry `json:"stakers"`
}

// GetStakerStatsResult is returned by the getstakerstats command.
type GetStakerStatsResult struct {
	LastDrawStakers    int `json:"lastdrawstakers"`    // unique wallets in the most recently closed draw
	Last10DrawsStakers int `json:"last10drawsstakers"` // unique wallets across the last 10 closed draws
	TotalStakers       int `json:"totalstakers"`       // unique wallets across every draw ever staked
}

// AddressUTXO describes a single unspent output for a wallet address.
type AddressUTXO struct {
	TxID          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	Atoms         int64  `json:"atoms"`
	Confirmations int32  `json:"confirmations"`
	Script        string `json:"script"`
	IsCoinbase    bool   `json:"iscoinbase"`
	IsStaking     bool   `json:"isstaking"`
	DrawHeight    uint32 `json:"drawheight,omitempty"`
}

// GetDrawTicketsResult is returned by the getdrawtickets command.
// Shows a wallet's token position range and the draw's winning position for
// independent verification of the SHA256-mod winner selection.
type GetDrawTicketsResult struct {
	WalletHash    string `json:"wallethash"`
	DrawHeight    uint32 `json:"drawheight"`
	DrawBlockHash string `json:"drawnblockhash"`
	EntropyHash   string `json:"entropyhash"` // DrawSeed: SHA256d(blockHash || vdfOutput[256])
	TotalTokens   int64  `json:"totaltokens"` // total real-staker tokens in this draw
	RangeStart    int64  `json:"rangestart"`  // wallet's first token position (inclusive)
	RangeEnd      int64  `json:"rangeend"`    // wallet's last token position + 1 (exclusive)
	WinningPos    int64  `json:"winningpos"`  // entropy mod totalTokens
	Won           bool   `json:"won"`
}

// CreateStakingOutputResult is returned by the createstakingoutput command.
type CreateStakingOutputResult struct {
	StakingScript    string `json:"stakingscript"`
	CommitmentScript string `json:"commitmentscript"`
}

// GetAddressBalanceResult is returned by the getaddressbalance command.
type GetAddressBalanceResult struct {
	Address       string  `json:"address"`
	SpendableFDRW float64 `json:"spendable"` // confirmed + unconfirmed normal UTXOs
	StakedFDRW    float64 `json:"staked"`    // locked in draw pools
	TotalFDRW     float64 `json:"total"`
}

// WalletStakeEntry is one draw in the result of getwalletstakehistory.
type WalletStakeEntry struct {
	DrawHeight uint32  `json:"drawheight"`
	FDRW       float64 `json:"fdrw"`
}

// GetWalletStakeHistoryResult is returned by the getwalletstakehistory command.
type GetWalletStakeHistoryResult struct {
	Entries []WalletStakeEntry `json:"entries"`
}

// AddressIncomingEntry describes one confirmed incoming transfer.
type AddressIncomingEntry struct {
	Txid        string  `json:"txid"`
	Atoms       int64   `json:"atoms"`
	FDRW        float64 `json:"fdrw"`
	BlockHeight int32   `json:"blockheight"`
}

// GetAddressIncomingResult is returned by the getaddressincoming command.
type GetAddressIncomingResult struct {
	Entries []AddressIncomingEntry `json:"entries"`
}
