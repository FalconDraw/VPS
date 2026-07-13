package blockchain

import (
	"encoding/binary"
	"sort"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/database"
	wire "github.com/btcsuite/btcd/wire/v2"
)

// ReconstructPoolBalance returns the true total pool (new stakes + accumulated
// jackpot rollover from prior draws) for drawHeight. If the pool balance was
// preserved in the DB (new behaviour after the fix), it is returned directly.
// For older settled draws where the balance was cleared to zero on rollover,
// it recursively sums new-stake atoms and traces the rollover chain backward
// to the nearest prior staker draw.
func (b *BlockChain) ReconstructPoolBalance(drawHeight uint32) int64 {
	// A settled draw always has its full accumulated pool stored (including any
	// carry-in from prior rollovers — runAndStoreDrawResult writes the full
	// amount via dbAbsorbIntermediateBalances before storing the result).
	_, _, found, _ := b.FetchDrawResultMeta(drawHeight)
	if found {
		return b.FetchPoolBalance(drawHeight)
	}

	// Unsettled draw (VDF pending or no stakers yet): compute from first
	// principles by summing new stakes and walking backward for carryIn.
	entries := b.FetchPoolStakeEntries(drawHeight)
	var newStakes int64
	for _, e := range entries {
		if e.WalletHash != JackpotPoolWalletHash {
			newStakes += e.TotalValue
		}
	}
	// Walk backward to find the nearest prior staker draw that rolled over.
	// Intermediate no-staker draws are skipped (their stranded balances are
	// accounted for within the prior staker draw's stored pool total after
	// runAndStoreDrawResult absorbs them).
	if drawHeight >= uint32(DrawInterval)*2 {
		for h := drawHeight - uint32(DrawInterval); h >= uint32(DrawInterval); h -= uint32(DrawInterval) {
			prevEntries := b.FetchPoolStakeEntries(h)
			hasStakers := false
			for _, e := range prevEntries {
				if e.WalletHash != JackpotPoolWalletHash {
					hasStakers = true
					break
				}
			}
			if !hasStakers {
				continue // no-staker pass-through draw, look further back
			}
			jackpotWon, _, priorFound, _ := b.FetchDrawResultMeta(h)
			if !priorFound || jackpotWon {
				break // not settled yet or jackpot won — rollover chain stops
			}
			return newStakes + b.ReconstructPoolBalance(h)
		}
	}
	return newStakes
}

// DrawStakeInfo is a wallet's aggregated stake position for a draw.
type DrawStakeInfo struct {
	WalletHash [20]byte
	Tokens     int64 // whole FDRW tokens (one per whole FDRW staked)
	TotalAtoms int64
}

// FetchDrawStakes returns staking positions for the given drawHeight from the
// pool stake index. Includes all real staker entries (no jackpot pool sentinels
// are ever inserted in pool staking).
func (b *BlockChain) FetchDrawStakes(drawHeight uint32) ([]DrawStakeInfo, error) {
	entries := b.FetchPoolStakeEntries(drawHeight)
	if len(entries) == 0 {
		return nil, nil
	}
	result := make([]DrawStakeInfo, 0, len(entries))
	for _, e := range entries {
		result = append(result, DrawStakeInfo{
			WalletHash: e.WalletHash,
			Tokens:     e.TokenCount,
			TotalAtoms: e.TotalValue,
		})
	}
	return result, nil
}

// FetchDrawStakingUTXOMap is a compatibility stub. Pool staking (v6) does not
// create staking UTXOs; the settlement builder uses pool DB state directly.
func (b *BlockChain) FetchDrawStakingUTXOMap(drawHeight uint32) (map[wire.OutPoint]*UtxoEntry, error) {
	return make(map[wire.OutPoint]*UtxoEntry), nil
}

// FetchDrawSettlement retrieves the settlement transaction and draw block hash
// for the draw at drawHeight. Returns nil, zero-hash, nil if the draw has not
// yet been settled (VDFResult not yet included in a block).
func (b *BlockChain) FetchDrawSettlement(drawHeight uint32) (*btcutil.Tx, chainhash.Hash, error) {
	// Look up the VDF settlement block height from the DB.
	settlementHeight, _, _, found := b.FetchVDFResult(drawHeight)
	if !found {
		return nil, chainhash.Hash{}, nil
	}
	if b.BestSnapshot().Height < settlementHeight {
		return nil, chainhash.Hash{}, nil
	}
	block, err := b.BlockByHeight(settlementHeight)
	if err != nil {
		return nil, chainhash.Hash{}, err
	}
	// Draw block hash = block at drawHeight.
	drawBlockNode := b.bestChain.NodeByHeight(int32(drawHeight))
	var drawBlockHash chainhash.Hash
	if drawBlockNode != nil {
		drawBlockHash = drawBlockNode.hash
	}
	txns := block.Transactions()
	for _, tx := range txns {
		if IsDrawSettlementTx(tx) {
			return tx, drawBlockHash, nil
		}
	}
	return nil, drawBlockHash, nil
}

// CountSettledDraws returns the number of draw blocks from DrawInterval up to
// and including upToHeight where at least one real staker participated.
// Rollover-only settlements with no real entrants are excluded.
func (b *BlockChain) CountSettledDraws(upToHeight uint32) (int, error) {
	if upToHeight < uint32(DrawInterval) {
		return 0, nil
	}
	if v, ok := b.drawSeqCache.Load(upToHeight); ok {
		return v.(int), nil
	}
	count := 0
	startH := uint32(DrawInterval)
	for h := upToHeight - uint32(DrawInterval); h >= uint32(DrawInterval); h -= uint32(DrawInterval) {
		if v, ok := b.drawSeqCache.Load(h); ok {
			count = v.(int)
			startH = h + uint32(DrawInterval)
			break
		}
	}
	for h := startH; h <= upToHeight; h += uint32(DrawInterval) {
		tx, _, err := b.FetchDrawSettlement(h)
		if err != nil {
			return 0, err
		}
		if tx == nil {
			_, _, _, vdfSettled := b.FetchVDFResult(h)
			if !vdfSettled {
				// VDF not yet settled — check for stakers to assign a sequence
				// number now (it won't change once stakers are committed).
				stakes, _ := b.FetchDrawStakes(h)
				if len(stakes) > 0 {
					count++
					// Don't cache: unsettled state shouldn't be stored permanently.
				}
				continue
			}
			// VDF settled but no settlement tx → zero-balance rollover draw.
			_, hasReal, liveErr := b.FetchDrawLiveResult(h)
			if liveErr == nil && hasReal {
				count++
			}
			b.drawSeqCache.Store(h, count)
			continue
		}
		_, hasReal, err := b.FetchDrawLiveResult(h)
		if err != nil {
			return 0, err
		}
		if hasReal {
			count++
		}
		b.drawSeqCache.Store(h, count)
	}
	return count, nil
}

// FetchDrawResultMeta returns the draw result for drawHeight.
// found is false if the draw block has not yet connected.
// FetchWalletStakeCount returns the number of confirmed pool stake transactions
// from walletHash that have been indexed for drawHeight.
func (b *BlockChain) FetchWalletStakeCount(drawHeight uint32, walletHash [20]byte) int {
	var count int
	b.db.View(func(dbTx database.Tx) error {
		count = dbGetWalletStakeCount(dbTx, drawHeight, walletHash)
		return nil
	})
	return count
}

func (b *BlockChain) FetchDrawResultMeta(drawHeight uint32) (jackpotWon bool, winnerWalletHash [20]byte, found bool, err error) {
	err = b.db.View(func(dbTx database.Tx) error {
		jackpotWon, winnerWalletHash, found = dbFetchDrawResultMeta(dbTx, drawHeight)
		return nil
	})
	return jackpotWon, winnerWalletHash, found, err
}

// FetchDrawAllStakers returns staking positions for a draw, excluding the
// jackpot pool sentinel. Multiple TXs from the same wallet are aggregated into
// a single entry. Pool stake entries persist in the DB after settlement, so
// both upcoming and settled draws read from the same index.
func (b *BlockChain) FetchDrawAllStakers(drawHeight uint32) ([]DrawStakeInfo, bool, error) {
	_, _, _, settled := b.FetchVDFResult(drawHeight)
	entries := b.FetchPoolStakeEntries(drawHeight)
	agg := make(map[[20]byte]*DrawStakeInfo)
	var order [][20]byte
	for _, e := range entries {
		if e.WalletHash == JackpotPoolWalletHash {
			continue
		}
		if s, ok := agg[e.WalletHash]; ok {
			s.Tokens += e.TokenCount
			s.TotalAtoms += e.TotalValue
		} else {
			d := &DrawStakeInfo{
				WalletHash: e.WalletHash,
				Tokens:     e.TokenCount,
				TotalAtoms: e.TotalValue,
			}
			agg[e.WalletHash] = d
			order = append(order, e.WalletHash)
		}
	}
	result := make([]DrawStakeInfo, 0, len(order))
	for _, wh := range order {
		result = append(result, *agg[wh])
	}
	return result, settled, nil
}

// FetchWalletTokensForDraw returns how many FDRW tokens a specific wallet
// staked in the given draw by reading pool stake entries.
func (b *BlockChain) FetchWalletTokensForDraw(walletHash [20]byte, drawHeight uint32) (int64, error) {
	entries := b.FetchPoolStakeEntries(drawHeight)
	var total int64
	for _, e := range entries {
		if e.WalletHash == walletHash {
			total += e.TokenCount
		}
	}
	return total, nil
}

// FetchDrawLiveResult re-derives the draw result for a settled draw from the
// pool stake index and drawResultMeta store.
// hasRealStakers is true when at least one pool stake entry exists for drawHeight.
func (b *BlockChain) FetchDrawLiveResult(drawHeight uint32) (jackpotWon bool, hasRealStakers bool, err error) {
	// A draw is settled once its VDF result has been stored.
	_, _, _, vdfSettled := b.FetchVDFResult(drawHeight)
	if !vdfSettled {
		return false, false, nil
	}
	entries := b.FetchPoolStakeEntries(drawHeight)
	hasRealStakers = len(entries) > 0
	if !hasRealStakers {
		return false, false, nil
	}
	jackpotWon, _, found, dbErr := b.FetchDrawResultMeta(drawHeight)
	if dbErr != nil {
		return false, hasRealStakers, dbErr
	}
	if found {
		return jackpotWon, hasRealStakers, nil
	}
	// Fall back to checking settlement tx outputs.
	tx, _, txErr := b.FetchDrawSettlement(drawHeight)
	if txErr != nil || tx == nil {
		return false, hasRealStakers, txErr
	}
	outs := tx.MsgTx().TxOut
	if len(outs) >= 2 {
		jackpotWon = true
	}
	return jackpotWon, hasRealStakers, nil
}

// StakerStats holds cumulative participation counts across all draws.
type StakerStats struct {
	LastDrawStakers    int // unique wallets in the most recently closed draw
	Last10DrawsStakers int // unique wallets across the last 10 closed draws
	TotalStakers       int // unique wallets across every draw ever staked (open or closed)
}

// FetchStakerStats performs a single scan of the pool stake index to compute
// cumulative staker counts: the most recently closed draw, the last 10 closed
// draws (deduplicated across draws), and the all-time unique wallet count.
// currentHeight is the current best chain height; a draw is "closed" once
// currentHeight has reached its drawHeight.
func (b *BlockChain) FetchStakerStats(currentHeight uint32) StakerStats {
	byDraw := make(map[uint32]map[[20]byte]struct{})
	allWallets := make(map[[20]byte]struct{})
	_ = b.db.View(func(dbTx database.Tx) error {
		bucket := dbTx.Metadata().Bucket(poolStakeByHeightBucketName)
		if bucket == nil {
			return nil
		}
		cursor := bucket.Cursor()
		for ok := cursor.First(); ok; ok = cursor.Next() {
			key := cursor.Key()
			val := cursor.Value()
			if len(key) < 36 || len(val) < 28 {
				continue
			}
			dh := binary.BigEndian.Uint32(key[:4])
			var wh [20]byte
			copy(wh[:], val[:20])
			if wh == JackpotPoolWalletHash {
				continue
			}
			wallets, ok := byDraw[dh]
			if !ok {
				wallets = make(map[[20]byte]struct{})
				byDraw[dh] = wallets
			}
			wallets[wh] = struct{}{}
			allWallets[wh] = struct{}{}
		}
		return nil
	})

	var closedHeights []uint32
	for dh := range byDraw {
		if dh <= currentHeight {
			closedHeights = append(closedHeights, dh)
		}
	}
	sort.Slice(closedHeights, func(i, j int) bool { return closedHeights[i] > closedHeights[j] })

	stats := StakerStats{TotalStakers: len(allWallets)}
	if len(closedHeights) == 0 {
		return stats
	}
	stats.LastDrawStakers = len(byDraw[closedHeights[0]])

	last10 := make(map[[20]byte]struct{})
	for i, dh := range closedHeights {
		if i >= 10 {
			break
		}
		for wh := range byDraw[dh] {
			last10[wh] = struct{}{}
		}
	}
	stats.Last10DrawsStakers = len(last10)
	return stats
}

// WalletDrawEntry is one draw in which a wallet has staked.
type WalletDrawEntry struct {
	DrawHeight uint32
	TotalAtoms int64
}

// FetchWalletStakeHistory returns all draws (past and future) in which the
// given wallet has pool stake entries. A single sequential scan of the stake
// DB is performed so this is O(total pool stake entries), not O(numDraws).
func (b *BlockChain) FetchWalletStakeHistory(walletHash [20]byte) []WalletDrawEntry {
	byDraw := make(map[uint32]int64)
	_ = b.db.View(func(dbTx database.Tx) error {
		bucket := dbTx.Metadata().Bucket(poolStakeByHeightBucketName)
		if bucket == nil {
			return nil
		}
		cursor := bucket.Cursor()
		for ok := cursor.First(); ok; ok = cursor.Next() {
			key := cursor.Key()
			val := cursor.Value()
			if len(key) < 36 || len(val) < 28 {
				continue
			}
			var wh [20]byte
			copy(wh[:], val[:20])
			if wh != walletHash {
				continue
			}
			dh := binary.BigEndian.Uint32(key[:4])
			atoms := int64(binary.BigEndian.Uint64(val[20:28]))
			byDraw[dh] += atoms
		}
		return nil
	})
	heights := make([]uint32, 0, len(byDraw))
	for h := range byDraw {
		heights = append(heights, h)
	}
	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
	result := make([]WalletDrawEntry, len(heights))
	for i, h := range heights {
		result[i] = WalletDrawEntry{DrawHeight: h, TotalAtoms: byDraw[h]}
	}
	return result
}
