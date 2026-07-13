package blockchain

import (
	"encoding/binary"
	"math/big"

	"github.com/btcsuite/btcd/btcutil/v2"
	chainhash "github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/database"
	wire "github.com/btcsuite/btcd/wire/v2"
)

// Pool staking indexes (TxVersionPoolStake / FDRP).
//
// poolStakeByHeightBucketName maps drawHeight(4)||txhash(32) → walletHash(20)||amount(8).
// poolBalanceBucketName maps drawHeight(4) → accumulated atoms (int64, 8 bytes).
var poolStakeByHeightBucketName = []byte("poolbyhgt")
var poolBalanceBucketName       = []byte("poolbal")

// VDF result index (TxVersionVDFResult / FDRV).
//
// vdfResultBucketName maps drawHeight(4) → settlementHeight(4) || settlementBlockHash(32) || T(8) || vdfOutput(256) = 300 bytes.
// Populated when a VDFResult tx is connected; cleared on disconnect.
// The settlement block hash lets validateVDFSettlement tell whether a
// recorded settlement is actually an ancestor of the block being validated,
// rather than trusting whatever chain currently happens to be connected in
// the db (see validateVDFSettlement for why that distinction matters).
//
// Legacy records written before the settlementBlockHash field was added are
// 268 bytes (no hash). They are only valid for settlement heights below the
// stored format cutoff (vdfFormatCutoffKeyName); at or above the cutoff every
// record must be the 300-byte format. See BlockChain.vdfFormatCutoff.
var vdfResultBucketName = []byte("vdfresult")

// vdfFormatCutoffKeyName stores the settlement height (int32, big-endian) at
// which the 300-byte VDF record format became mandatory. Written once, the
// first time a node with hash-aware code starts: cutoff = bestHeight+1, so
// every record below it is grandfathered legacy history and every record at
// or above it must carry a settlement block hash.
var vdfFormatCutoffKeyName = []byte("vdfresultcutoff")

// stakeWalletCountBucketName maps drawHeight(4)||walletHash(20) → count(1).
// Tracks how many pool stake transactions a wallet has submitted per draw.
var stakeWalletCountBucketName = []byte("stakecnt")

// connectBlockStakingIndex updates the staking index when a block is connected.
//
// Pool staking TXs (v6) are indexed by draw height and their amount credited
// to the pool balance.
//
// VDFResult TXs (v7) trigger the draw and store the result so that settlement
// validation in the same block can read it O(1).  Settlement happens in the
// same block as the VDFResult — draw blocks no longer trigger the draw
// immediately; the draw runs only when the VDF proof arrives.
func connectBlockStakingIndex(dbTx database.Tx, block *btcutil.Block, stxos []SpentTxOut) error {
	blockHeight := block.Height()
	for _, tx := range block.Transactions() {
		switch tx.MsgTx().Version {

		case wire.TxVersionPoolStake:
			txHash := tx.Hash()
			opReturnIdx, script := FindPoolStakingOpReturn(tx.MsgTx())
			if opReturnIdx < 0 {
				continue
			}
			wh, amt, dh, err := ExtractPoolStakingData(script)
			if err != nil {
				continue
			}
			// If the target draw has already fired (or is the current block),
			// remap to the next draw. This lets stakes land even when blocks
			// mine faster than the UI can react.
			if int32(dh) <= blockHeight {
				dh = uint32((blockHeight/int32(DrawInterval)+1) * int32(DrawInterval))
			}
			// Enforce per-wallet stake limit. Excess stakes are skipped —
			// they are valid transactions (fees paid) but do not enter the
			// draw index or pool balance.
			if dbGetWalletStakeCount(dbTx, dh, wh) >= MaxStakesPerWalletPerDraw {
				continue
			}
			if err := dbPutPoolStakeEntry(dbTx, dh, txHash, wh, amt); err != nil {
				return err
			}
			if err := dbAddToPoolBalance(dbTx, dh, amt); err != nil {
				return err
			}
			if err := dbIncrWalletStakeCount(dbTx, dh, wh); err != nil {
				return err
			}

		case wire.TxVersionVDFResult:
			drawHeight, drawBlockHash, T, vdfOutput, _, err := ExtractVDFResultData(tx)
			if err != nil {
				continue
			}
			if err := dbPutVDFResult(dbTx, drawHeight, blockHeight, *block.Hash(), T, vdfOutput); err != nil {
				return err
			}
			// Compute draw seed from the VDF proof and run the draw.
			var blockHashArr chainhash.Hash
			copy(blockHashArr[:], drawBlockHash[:])
			seed := DrawSeed(blockHashArr, vdfOutput)
			if err := runAndStoreDrawResult(dbTx, drawHeight, seed); err != nil {
				return err
			}
		}
	}
	return nil
}

// runAndStoreDrawResult executes RunDraw for drawHeight using the VDF-derived
// seed and pool stake entries already in the DB.  Stores the result
// (jackpotWon + winnerWalletHash) and handles rollover.  Called when the
// VDFResult tx connects (same block as settlement).
func runAndStoreDrawResult(dbTx database.Tx, drawHeight uint32, seed [32]byte) error {
	entries := dbFetchPoolStakeEntries(dbTx, drawHeight)
	if len(entries) == 0 {
		// No real stakers.  Roll over any accumulated pool balance from previous
		// draws so it isn't stranded at this height forever.
		bal := dbGetPoolBalance(dbTx, drawHeight)
		if bal > 0 {
			if err := dbPutDrawResultMeta(dbTx, drawHeight, false, [20]byte{}); err != nil {
				return err
			}
			nextDraw := drawHeight + uint32(DrawInterval)
			if err := dbAddToPoolBalance(dbTx, nextDraw, bal); err != nil {
				return err
			}
			if err := dbSetPoolBalance(dbTx, drawHeight, 0); err != nil {
				return err
			}
		}
		return nil
	}

	// Staker draw: absorb any stranded intermediate no-staker balances into the
	// full pool so the historical record and rollover carry the correct total.
	fullBal := dbAbsorbIntermediateBalances(dbTx, drawHeight)
	if err := dbSetPoolBalance(dbTx, drawHeight, fullBal); err != nil {
		return err
	}

	stakes := make([]StakeEntry, 0, len(entries))
	for _, e := range entries {
		stakes = append(stakes, StakeEntry{
			WalletHash: e.WalletHash,
			TokenCount: e.Amount / btcutil.SatoshiPerBitcoin,
			TotalValue: e.Amount,
		})
	}

	result := RunDraw(stakes, seed)

	var winnerWalletHash [20]byte
	if result.JackpotWon && len(result.Winners) > 0 {
		winnerWalletHash = result.Winners[0].WalletHash
	}
	if err := dbPutDrawResultMeta(dbTx, drawHeight, result.JackpotWon, winnerWalletHash); err != nil {
		return err
	}

	// Rollover: carry the full accumulated pool to the next draw.  Balance is
	// left at drawHeight (not cleared) as the permanent historical record.
	if !result.JackpotWon && fullBal > 0 {
		nextDraw := drawHeight + uint32(DrawInterval)
		if err := dbAddToPoolBalance(dbTx, nextDraw, fullBal); err != nil {
			return err
		}
	}
	return nil
}

// dbAbsorbIntermediateBalances walks backward from drawHeight collecting any
// stranded pool balances sitting at intermediate no-staker draw heights (draws
// where no VDF ran so runAndStoreDrawResult was never called).  Those balances
// are cleared from their original locations and folded into the returned total
// for drawHeight, which includes the draw's own new stakes.
func dbAbsorbIntermediateBalances(dbTx database.Tx, drawHeight uint32) int64 {
	total := dbGetPoolBalance(dbTx, drawHeight) // own new stakes
	if drawHeight < uint32(DrawInterval)*2 {
		return total
	}
	for h := drawHeight - uint32(DrawInterval); h >= uint32(DrawInterval); h -= uint32(DrawInterval) {
		hBal := dbGetPoolBalance(dbTx, h)
		if hBal == 0 {
			continue
		}
		if len(dbFetchPoolStakeEntries(dbTx, h)) > 0 {
			// Found a prior staker draw.  Its rollover was already propagated
			// into the intermediate draw heights we traversed above (via
			// dbAddToPoolBalance during that draw's settlement).  Those
			// intermediate balances are already included in total, so we must
			// NOT add hBal again.  Stop scanning.
			break
		}
		// Intermediate no-staker draw with a stranded balance: absorb and clear
		// to avoid double-counting on future calls.
		total += hBal
		_ = dbSetPoolBalance(dbTx, h, 0)
	}
	return total
}

// ── VDF result index ──────────────────────────────────────────────────────────

func dbCreateVDFResultBucket(dbTx database.Tx) error {
	_, err := dbTx.Metadata().CreateBucket(vdfResultBucketName)
	return err
}

func dbPutVDFResult(dbTx database.Tx, drawHeight uint32, settlementHeight int32, settlementBlockHash chainhash.Hash, T uint64, output *big.Int) error {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], drawHeight)
	val := make([]byte, 4+32+8+256)
	binary.BigEndian.PutUint32(val[:4], uint32(settlementHeight))
	copy(val[4:36], settlementBlockHash[:])
	binary.BigEndian.PutUint64(val[36:44], T)
	output.FillBytes(val[44:]) // zero-pads to 256 bytes
	return dbTx.Metadata().Bucket(vdfResultBucketName).Put(key[:], val)
}

// dbGetVDFResult reads a VDF settlement record, accepting both formats.
// hasHash reports whether the record carries a settlement block hash (new
// 300-byte format); legacy 268-byte records return a zero hash and
// hasHash=false.
func dbGetVDFResult(dbTx database.Tx, drawHeight uint32) (settlementHeight int32, settlementBlockHash chainhash.Hash, hasHash bool, T uint64, output *big.Int, found bool) {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], drawHeight)
	val := dbTx.Metadata().Bucket(vdfResultBucketName).Get(key[:])
	switch {
	case len(val) >= 300: // new format with settlement block hash
		settlementHeight = int32(binary.BigEndian.Uint32(val[:4]))
		copy(settlementBlockHash[:], val[4:36])
		T = binary.BigEndian.Uint64(val[36:44])
		output = new(big.Int).SetBytes(val[44:])
		return settlementHeight, settlementBlockHash, true, T, output, true
	case len(val) >= 268: // legacy format, no hash
		settlementHeight = int32(binary.BigEndian.Uint32(val[:4]))
		T = binary.BigEndian.Uint64(val[4:12])
		output = new(big.Int).SetBytes(val[12:])
		return settlementHeight, chainhash.Hash{}, false, T, output, true
	default:
		return 0, chainhash.Hash{}, false, 0, nil, false
	}
}

func dbDeleteVDFResult(dbTx database.Tx, drawHeight uint32) error {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], drawHeight)
	return dbTx.Metadata().Bucket(vdfResultBucketName).Delete(key[:])
}

// FetchVDFResult returns the stored VDF result for drawHeight.
// Returns (0, 0, nil, false) if no VDF result has been stored yet.
func (b *BlockChain) FetchVDFResult(drawHeight uint32) (settlementHeight int32, T uint64, output *big.Int, found bool) {
	settlementHeight, _, _, T, output, found = b.fetchVDFSettlement(drawHeight)
	return
}

// fetchVDFSettlement is like FetchVDFResult but also returns the hash of the
// block that recorded the settlement (hasHash=false for legacy pre-cutoff
// records), so callers can verify the settlement is actually part of the
// chain they're validating (see validateVDFSettlement).
func (b *BlockChain) fetchVDFSettlement(drawHeight uint32) (settlementHeight int32, settlementBlockHash chainhash.Hash, hasHash bool, T uint64, output *big.Int, found bool) {
	_ = b.db.View(func(dbTx database.Tx) error {
		settlementHeight, settlementBlockHash, hasHash, T, output, found = dbGetVDFResult(dbTx, drawHeight)
		return nil
	})
	return
}

// dbFetchVDFFormatCutoff returns the stored VDF record format cutoff height,
// or -1 if none has been stored yet.
func dbFetchVDFFormatCutoff(dbTx database.Tx) int32 {
	val := dbTx.Metadata().Get(vdfFormatCutoffKeyName)
	if len(val) < 4 {
		return -1
	}
	return int32(binary.BigEndian.Uint32(val))
}

// dbPutVDFFormatCutoff persists the VDF record format cutoff height.
func dbPutVDFFormatCutoff(dbTx database.Tx, cutoff int32) error {
	var val [4]byte
	binary.BigEndian.PutUint32(val[:], uint32(cutoff))
	return dbTx.Metadata().Put(vdfFormatCutoffKeyName, val[:])
}

// ── Draw result metadata ─────────────────────────────────────────────────────
//
// drawResultMetaBucketName stores per-draw {jackpotWon uint8} so
// getdrawresult can answer quickly without re-running the full draw.
// Key: drawHeight[4 big-endian]; Value: jackpotWon[1] (1=won, 0=rollover).

var drawResultMetaBucketName = []byte("drawresultmeta")

func dbCreateDrawResultMetaBucket(dbTx database.Tx) error {
	_, err := dbTx.Metadata().CreateBucket(drawResultMetaBucketName)
	return err
}

func dbCreatePoolStakeByHeightBucket(dbTx database.Tx) error {
	_, err := dbTx.Metadata().CreateBucket(poolStakeByHeightBucketName)
	return err
}

func dbCreateStakeWalletCountBucket(dbTx database.Tx) error {
	_, err := dbTx.Metadata().CreateBucket(stakeWalletCountBucketName)
	return err
}

// stakeWalletCountKey returns the key for drawHeight(4)||walletHash(20).
func stakeWalletCountKey(drawHeight uint32, walletHash [20]byte) [24]byte {
	var key [24]byte
	binary.BigEndian.PutUint32(key[:4], drawHeight)
	copy(key[4:], walletHash[:])
	return key
}

// dbGetWalletStakeCount returns the number of confirmed pool stakes from
// walletHash for drawHeight (0 if none).
func dbGetWalletStakeCount(dbTx database.Tx, drawHeight uint32, walletHash [20]byte) int {
	bucket := dbTx.Metadata().Bucket(stakeWalletCountBucketName)
	if bucket == nil {
		return 0
	}
	key := stakeWalletCountKey(drawHeight, walletHash)
	v := bucket.Get(key[:])
	if len(v) == 0 {
		return 0
	}
	return int(v[0])
}

// dbIncrWalletStakeCount increments the confirmed stake count for
// walletHash at drawHeight, capping at 255.
func dbIncrWalletStakeCount(dbTx database.Tx, drawHeight uint32, walletHash [20]byte) error {
	key := stakeWalletCountKey(drawHeight, walletHash)
	bucket := dbTx.Metadata().Bucket(stakeWalletCountBucketName)
	cur := 0
	if v := bucket.Get(key[:]); len(v) > 0 {
		cur = int(v[0])
	}
	if cur < 255 {
		cur++
	}
	return bucket.Put(key[:], []byte{byte(cur)})
}

func dbCreatePoolBalanceBucket(dbTx database.Tx) error {
	_, err := dbTx.Metadata().CreateBucket(poolBalanceBucketName)
	return err
}

// ── Pool staking index ────────────────────────────────────────────────────────

// poolStakeKey returns the key for a pool stake entry: drawHeight(4)||txhash(32).
func poolStakeKey(drawHeight uint32, txHash *chainhash.Hash) [36]byte {
	var key [36]byte
	binary.BigEndian.PutUint32(key[:4], drawHeight)
	copy(key[4:36], txHash[:])
	return key
}

// dbPutPoolStakeEntry indexes a pool staking TX under its draw height.
// value: walletHash(20) || amount(8) = 28 bytes.
func dbPutPoolStakeEntry(dbTx database.Tx, drawHeight uint32, txHash *chainhash.Hash, walletHash [20]byte, amount int64) error {
	key := poolStakeKey(drawHeight, txHash)
	val := make([]byte, 28)
	copy(val[:20], walletHash[:])
	binary.BigEndian.PutUint64(val[20:28], uint64(amount))
	return dbTx.Metadata().Bucket(poolStakeByHeightBucketName).Put(key[:], val)
}

// dbRemovePoolStakeEntry removes a pool stake entry (on block disconnect).
func dbRemovePoolStakeEntry(dbTx database.Tx, drawHeight uint32, txHash *chainhash.Hash) error {
	key := poolStakeKey(drawHeight, txHash)
	return dbTx.Metadata().Bucket(poolStakeByHeightBucketName).Delete(key[:])
}

// dbFetchPoolStakeEntries returns all pool stake entries for a draw height.
// Returns a slice of (txHash, walletHash, amount).
type poolStakeEntry struct {
	TxHash     chainhash.Hash
	WalletHash [20]byte
	Amount     int64 // atoms
}

func dbFetchPoolStakeEntries(dbTx database.Tx, drawHeight uint32) []poolStakeEntry {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], drawHeight)
	cursor := dbTx.Metadata().Bucket(poolStakeByHeightBucketName).Cursor()
	var entries []poolStakeEntry
	for ok := cursor.Seek(prefix[:]); ok; ok = cursor.Next() {
		key := cursor.Key()
		if len(key) < 36 || binary.BigEndian.Uint32(key[:4]) != drawHeight {
			break
		}
		val := cursor.Value()
		if len(val) < 28 {
			continue
		}
		var e poolStakeEntry
		copy(e.TxHash[:], key[4:36])
		copy(e.WalletHash[:], val[:20])
		e.Amount = int64(binary.BigEndian.Uint64(val[20:28]))
		entries = append(entries, e)
	}
	return entries
}

// dbHasPoolStakeEntries returns true if any pool stake entries exist for drawHeight.
func dbHasPoolStakeEntries(dbTx database.Tx, drawHeight uint32) bool {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], drawHeight)
	cursor := dbTx.Metadata().Bucket(poolStakeByHeightBucketName).Cursor()
	if !cursor.Seek(prefix[:]) {
		return false
	}
	key := cursor.Key()
	return len(key) >= 4 && binary.BigEndian.Uint32(key[:4]) == drawHeight
}

// ── Pool balance index ────────────────────────────────────────────────────────

func dbGetPoolBalance(dbTx database.Tx, drawHeight uint32) int64 {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], drawHeight)
	v := dbTx.Metadata().Bucket(poolBalanceBucketName).Get(key[:])
	if len(v) < 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(v))
}

func dbSetPoolBalance(dbTx database.Tx, drawHeight uint32, atoms int64) error {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], drawHeight)
	var val [8]byte
	binary.BigEndian.PutUint64(val[:], uint64(atoms))
	return dbTx.Metadata().Bucket(poolBalanceBucketName).Put(key[:], val[:])
}

func dbAddToPoolBalance(dbTx database.Tx, drawHeight uint32, delta int64) error {
	cur := dbGetPoolBalance(dbTx, drawHeight)
	return dbSetPoolBalance(dbTx, drawHeight, cur+delta)
}

// FetchPoolBalance returns the accumulated pool balance for a draw height.
func (b *BlockChain) FetchPoolBalance(drawHeight uint32) int64 {
	var bal int64
	_ = b.db.View(func(dbTx database.Tx) error {
		bal = dbGetPoolBalance(dbTx, drawHeight)
		return nil
	})
	return bal
}

// FetchPoolStakeEntries returns stake entries for drawHeight as StakeEntry
// slices ready for RunDraw.
func (b *BlockChain) FetchPoolStakeEntries(drawHeight uint32) []StakeEntry {
	var raw []poolStakeEntry
	_ = b.db.View(func(dbTx database.Tx) error {
		raw = dbFetchPoolStakeEntries(dbTx, drawHeight)
		return nil
	})
	out := make([]StakeEntry, 0, len(raw))
	for _, e := range raw {
		out = append(out, StakeEntry{
			WalletHash: e.WalletHash,
			TokenCount: e.Amount / btcutil.SatoshiPerBitcoin,
			TotalValue: e.Amount,
		})
	}
	return out
}


func dbDrawResultMetaBucket(dbTx database.Tx) database.Bucket {
	return dbTx.Metadata().Bucket(drawResultMetaBucketName)
}

// dbPutDrawResultMeta stores jackpotWon(1) || winnerWalletHash(20) for a draw.
func dbPutDrawResultMeta(dbTx database.Tx, drawHeight uint32, jackpotWon bool, winnerWalletHash [20]byte) error {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], drawHeight)
	var val [21]byte
	if jackpotWon {
		val[0] = 1
	}
	copy(val[1:], winnerWalletHash[:])
	return dbDrawResultMetaBucket(dbTx).Put(key[:], val[:])
}

func dbDeleteDrawResultMeta(dbTx database.Tx, drawHeight uint32) error {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], drawHeight)
	return dbDrawResultMetaBucket(dbTx).Delete(key[:])
}

func dbFetchDrawResultMeta(dbTx database.Tx, drawHeight uint32) (jackpotWon bool, winnerWalletHash [20]byte, found bool) {
	var key [4]byte
	binary.BigEndian.PutUint32(key[:], drawHeight)
	b := dbDrawResultMetaBucket(dbTx)
	if b == nil {
		return false, winnerWalletHash, false
	}
	val := b.Get(key[:])
	if len(val) < 21 {
		return false, winnerWalletHash, false
	}
	copy(winnerWalletHash[:], val[1:21])
	return val[0] == 1, winnerWalletHash, true
}



// ── Disconnect: reverse draw result metadata ─────────────────────────────────

// disconnectBlockStakingIndex reverses the staking index update for a block
// being disconnected.  Pool stake entries are removed and pool balance debited.
// VDFResult txs in the disconnecting block reverse the draw result and rollover.
func disconnectBlockStakingIndex(dbTx database.Tx, block *btcutil.Block, stxos []SpentTxOut) error {
	for _, tx := range block.Transactions() {
		switch tx.MsgTx().Version {

		case wire.TxVersionPoolStake:
			txHash := tx.Hash()
			opReturnIdx, script := FindPoolStakingOpReturn(tx.MsgTx())
			if opReturnIdx < 0 {
				continue
			}
			_, amt, dh, err := ExtractPoolStakingData(script)
			if err != nil {
				continue
			}
			if err := dbRemovePoolStakeEntry(dbTx, dh, txHash); err != nil {
				return err
			}
			if err := dbAddToPoolBalance(dbTx, dh, -amt); err != nil {
				return err
			}

		case wire.TxVersionVDFResult:
			drawHeight, _, _, _, _, err := ExtractVDFResultData(tx)
			if err != nil {
				continue
			}
			// Remove VDF result.
			if err := dbDeleteVDFResult(dbTx, drawHeight); err != nil {
				return err
			}
			// Reverse draw result and rollover.
			jackpotWon, _, found := dbFetchDrawResultMeta(dbTx, drawHeight)
			if found {
				if err := dbDeleteDrawResultMeta(dbTx, drawHeight); err != nil {
					return err
				}
				if !jackpotWon {
					nextDraw := drawHeight + uint32(DrawInterval)
					rolledBal := dbGetPoolBalance(dbTx, nextDraw)
					if rolledBal > 0 {
						if err := dbSetPoolBalance(dbTx, drawHeight, rolledBal); err != nil {
							return err
						}
						if err := dbSetPoolBalance(dbTx, nextDraw, 0); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// AuditPoolBalances scans the pool-balance index at startup and logs
// anomalies.  It is strictly read-only.
//
// It replaces RepairStrandedPoolBalances, which mutated pool balances at
// startup by forwarding stranded amounts to the "next upcoming draw" — a
// height that depends on when the node happens to restart.  Pool balances
// feed jackpot settlement validation (validatePoolDrawSettlementTx), so any
// startup-time mutation makes a node's consensus state a function of its
// restart history rather than of the chain.  Two nodes with different
// restart schedules silently diverge and the split only surfaces when a
// jackpot settlement is minted (2026-07-10: 300 FDRW mismatch at block
// 47850 forked the VPS node off the network).
//
// No repair is needed: dbAbsorbIntermediateBalances already folds stranded
// no-staker balances into the next staker draw's settlement, driven by
// block connects and therefore identical on every node.
func (b *BlockChain) AuditPoolBalances() error {
	best := b.BestSnapshot().Height
	if best < int32(DrawInterval) {
		return nil
	}

	return b.db.View(func(dbTx database.Tx) error {
		for h := uint32(DrawInterval); int32(h) <= best; h += uint32(DrawInterval) {
			bal := dbGetPoolBalance(dbTx, h)
			_, _, _, _, _, settled := dbGetVDFResult(dbTx, h)
			hasStakers := len(dbFetchPoolStakeEntries(dbTx, h)) > 0

			// Unsettled staker draw with a zero balance: the balance was lost
			// (legacy startup-repair damage or index corruption).  The next
			// settlement built from this state will disagree with any node
			// that recomputes from chain data — resync to rebuild the index.
			if !settled && hasStakers && bal == 0 {
				var stakeSum int64
				for _, e := range dbFetchPoolStakeEntries(dbTx, h) {
					stakeSum += e.Amount
				}
				if stakeSum > 0 {
					log.Errorf("Pool audit: draw %d has %d atoms of stake "+
						"entries but a zero pool balance — index is "+
						"inconsistent with chain data; resync recommended",
						h, stakeSum)
				}
				continue
			}

			// Unsettled no-staker draw holding a balance: normal transient
			// state — dbAbsorbIntermediateBalances folds it into the next
			// staker draw's settlement.
			if !settled && !hasStakers && bal > 0 {
				log.Infof("Pool audit: %d atoms parked at no-staker draw %d, "+
					"pending absorption by the next staker settlement", bal, h)
			}
		}
		return nil
	})
}
