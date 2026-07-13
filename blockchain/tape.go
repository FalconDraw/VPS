package blockchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/big"
	"sort"

	chainhash "github.com/btcsuite/btcd/chainhash/v2"
)

// StakeEntry is a wallet's aggregated stake position for a draw.
// Multiple staking UTXOs from the same wallet are merged into one entry.
// Jackpot accumulation UTXOs (WalletHash == JackpotPoolWalletHash) are
// included for pool accounting but excluded from winner selection.
type StakeEntry struct {
	WalletHash [20]byte        // staker pubkey hash (all-zeros = jackpot pool)
	TokenCount int64           // total whole-FDRW tokens staked
	TotalValue int64           // total atoms staked across all UTXOs
}

// WinnerEntry is a draw participant who won the prize.
type WinnerEntry struct {
	WalletHash [20]byte // staker pubkey hash
	Prize      int64    // atoms paid to this winner (full pool for jackpot; split for ties)
}

// DrawResult is the complete outcome of a Falcon Draw.
type DrawResult struct {
	JackpotWon     bool          // true if the jackpot was awarded this draw
	Winners        []WinnerEntry // prize recipients (normally 1; >1 only if tied)
	Rollover       int64         // atoms to re-lock for the next draw (0 if jackpot won)
	WinningPos     int64         // winning token position (0 = no draw or rollover)
	TotalTokens    int64         // total real staker tokens that participated
}

// WinMultiplier is the tape length multiplier that controls draw win frequency.
// With WinMultiplier=10, each draw has exactly a 1-in-10 (10%) chance of
// producing a jackpot winner. Raise to reduce frequency; lower to increase it.
const WinMultiplier = 10

// CalcTargetHash computes the pre-VDF draw entropy: SHA256d(blockNHash || drawHeight_be4).
// Retained for tests; production draws use DrawSeed (vdf.go) as entropy.
func CalcTargetHash(blockNHash chainhash.Hash, drawHeight uint32) [32]byte {
	var buf [36]byte
	copy(buf[:32], blockNHash[:])
	binary.BigEndian.PutUint32(buf[32:36], drawHeight)
	first := sha256.Sum256(buf[:])
	return sha256.Sum256(first[:])
}

// SelectWinnerPos returns the winning token position given entropy and total tokens.
// winning_pos = bigint(entropy) mod totalTokens
func SelectWinnerPos(entropy [32]byte, totalTokens int64) int64 {
	var n big.Int
	n.SetBytes(entropy[:])
	var total big.Int
	total.SetInt64(totalTokens)
	var pos big.Int
	pos.Mod(&n, &total)
	return pos.Int64()
}

// cumulativeEntry is one wallet in the sorted deposit list, with its
// [start, end) token range. The winner is the wallet whose range contains
// the winning position.
type cumulativeEntry struct {
	walletHash [20]byte
	totalValue int64
	start      int64 // inclusive lower bound of this wallet's token range
	end        int64 // exclusive upper bound
}

// buildSortedDepositList sorts real stakers by wallet hash and builds the
// cumulative token range list used for O(log N) winner lookup.
func buildSortedDepositList(real []StakeEntry) []cumulativeEntry {
	sorted := make([]StakeEntry, len(real))
	copy(sorted, real)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].WalletHash[:], sorted[j].WalletHash[:]) < 0
	})
	entries := make([]cumulativeEntry, len(sorted))
	var cumulative int64
	for i, s := range sorted {
		entries[i] = cumulativeEntry{
			walletHash: s.WalletHash,
			totalValue: s.TotalValue,
			start:      cumulative,
			end:        cumulative + s.TokenCount,
		}
		cumulative += s.TokenCount
	}
	return entries
}

// findWinner binary-searches the cumulative list for the wallet that owns pos.
func findWinner(entries []cumulativeEntry, pos int64) (cumulativeEntry, bool) {
	lo, hi := 0, len(entries)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if pos < entries[mid].start {
			hi = mid - 1
		} else if pos >= entries[mid].end {
			lo = mid + 1
		} else {
			return entries[mid], true
		}
	}
	return cumulativeEntry{}, false
}

// RunDraw executes the Falcon Draw using a pre-computed entropy seed.
//
// seed is DrawSeed(drawBlockHash, vdfOutput) — derived from the VDF result so
// miners cannot grind the block hash to predict outcomes before committing.
//
// stakes must include all staking entries for the draw height.
//
// Winner selection (tape mechanic):
//  1. extended_pos = bigint(seed) mod (WinMultiplier × totalRealTokens)
//  2. if extended_pos >= totalRealTokens → rollover
//  3. else binary search sorted-by-walletHash deposit list → winning wallet
//  4. winner takes 100% of the accumulated pool
func RunDraw(stakes []StakeEntry, seed [32]byte) DrawResult {
	if len(stakes) == 0 {
		return DrawResult{}
	}

	var realStakes []StakeEntry
	var totalPool, totalRealTokens int64
	for _, s := range stakes {
		totalPool += s.TotalValue
		if s.WalletHash != JackpotPoolWalletHash {
			realStakes = append(realStakes, s)
			totalRealTokens += s.TokenCount
		}
	}

	if totalPool <= 0 {
		return DrawResult{}
	}
	if len(realStakes) == 0 {
		return DrawResult{Rollover: totalPool}
	}

	// Tape mechanic: extend position space by WinMultiplier.
	// Position in [0, totalRealTokens) → jackpot; beyond → rollover.
	extendedPos := SelectWinnerPos(seed, totalRealTokens*WinMultiplier)
	if extendedPos >= totalRealTokens {
		return DrawResult{Rollover: totalPool, TotalTokens: totalRealTokens}
	}

	entries := buildSortedDepositList(realStakes)
	winner, ok := findWinner(entries, extendedPos)
	if !ok {
		// Should never happen: extendedPos is in [0, totalRealTokens).
		return DrawResult{Rollover: totalPool, TotalTokens: totalRealTokens}
	}

	return DrawResult{
		JackpotWon:  true,
		Winners:     []WinnerEntry{{WalletHash: winner.walletHash, Prize: totalPool}},
		WinningPos:  extendedPos,
		TotalTokens: totalRealTokens,
	}
}


// P2PKHScript builds the standard P2PKH locking script for a pubkey hash.
func P2PKHScript(hash [20]byte) []byte {
	script := make([]byte, 25)
	script[0] = 0x76 // OP_DUP
	script[1] = 0xa9 // OP_HASH160
	script[2] = 0x14 // OP_DATA_20
	copy(script[3:23], hash[:])
	script[23] = 0x88 // OP_EQUALVERIFY
	script[24] = 0xac // OP_CHECKSIG
	return script
}
