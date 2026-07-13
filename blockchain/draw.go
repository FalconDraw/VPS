package blockchain

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	wire "github.com/btcsuite/btcd/wire/v2"
)

// Draw settlement TX marker layout (10-byte OP_RETURN output pkScript):
//
//	[0]    0x6a — OP_RETURN
//	[1]    0x08 — push 8 bytes
//	[2:6]  "FDRS" magic
//	[6:10] draw block height, big-endian uint32
const (
	DrawSettlementMarkerSize = 10
	drawSettlementDataSize   = 8
)

var drawSettlementMagic = [4]byte{'F', 'D', 'R', 'S'}

// MakeDrawSettlementMarker builds the OP_RETURN pkScript for the zeroth output
// of every draw settlement transaction.
func MakeDrawSettlementMarker(drawHeight uint32) []byte {
	script := make([]byte, DrawSettlementMarkerSize)
	script[0] = 0x6a // OP_RETURN
	script[1] = 0x08 // push 8 bytes
	copy(script[2:6], drawSettlementMagic[:])
	binary.BigEndian.PutUint32(script[6:10], drawHeight)
	return script
}

// extractDrawSettlementMarker parses a draw settlement marker pkScript.
// Returns (drawHeight, true) on success, (0, false) otherwise.
func extractDrawSettlementMarker(pkScript []byte) (uint32, bool) {
	if len(pkScript) != DrawSettlementMarkerSize {
		return 0, false
	}
	if pkScript[0] != 0x6a || pkScript[1] != 0x08 {
		return 0, false
	}
	if pkScript[2] != drawSettlementMagic[0] ||
		pkScript[3] != drawSettlementMagic[1] ||
		pkScript[4] != drawSettlementMagic[2] ||
		pkScript[5] != drawSettlementMagic[3] {
		return 0, false
	}
	return binary.BigEndian.Uint32(pkScript[6:10]), true
}

// IsDrawSettlementTx returns true if tx is a Falcon Draw settlement transaction.
// Draw settlements are the ONLY transactions permitted to spend staking outputs;
// all other attempts to spend a staking UTXO are rejected by consensus.
//
// A draw settlement is recognised by its first output carrying the FDRS
// OP_RETURN marker built by MakeDrawSettlementMarker.
func IsDrawSettlementTx(tx *btcutil.Tx) bool {
	outs := tx.MsgTx().TxOut
	if len(outs) == 0 {
		return false
	}
	_, ok := extractDrawSettlementMarker(outs[0].PkScript)
	return ok
}

// PoolDrawData bundles the O(1) pool-staking state needed for settlement
// validation and building. All fields come from single DB key reads.
type PoolDrawData struct {
	Balance          int64    // accumulated pool atoms for this draw
	JackpotWon       bool     // true if the draw produced a winner
	WinnerWalletHash [20]byte // winning wallet hash (zero if no jackpot)
}

// validatePoolDrawSettlementTx validates a settlement TX O(1) against the
// stored draw result. The TX must have:
//   - Output[0]: FDRS marker for drawHeight
//   - Output[1]: P2WPKH output for winning walletHash (jackpot only)
func validatePoolDrawSettlementTx(msgTx *wire.MsgTx, drawHeight uint32, pd PoolDrawData) error {
	if pd.Balance == 0 && !pd.JackpotWon {
		return ruleError(ErrBadDrawSettlementTx,
			"pool draw settlement: no pool balance")
	}

	wantOuts := 1 // marker only (rollover)
	if pd.JackpotWon {
		wantOuts = 2 // marker + winner
	}
	if len(msgTx.TxOut) != wantOuts {
		return ruleError(ErrBadDrawSettlementTx,
			fmt.Sprintf("pool draw settlement has %d outputs; expected %d (%s)",
				len(msgTx.TxOut), wantOuts,
				map[bool]string{true: "marker + winner", false: "marker only (rollover)"}[pd.JackpotWon]))
	}

	if pd.JackpotWon {
		out := msgTx.TxOut[1]
		if out.Value != pd.Balance {
			return ruleError(ErrBadDrawSettlementTx,
				fmt.Sprintf("pool draw settlement winner output value %d != pool balance %d",
					out.Value, pd.Balance))
		}
		wantScript := MakeWinnerP2WPKHScript(pd.WinnerWalletHash)
		if !bytes.Equal(out.PkScript, wantScript) {
			return ruleError(ErrBadDrawSettlementTx,
				"pool draw settlement winner output script does not match winner P2WPKH script")
		}
	}

	return nil
}

// ValidateDrawSettlementTx performs O(1) consensus validation of a draw
// settlement transaction.  drawHeight is the draw block height (N).
// pd is computed inline from the VDF-derived draw result.
func ValidateDrawSettlementTx(tx *btcutil.Tx, drawHeight uint32, view *UtxoViewpoint, pd PoolDrawData) error {
	msgTx := tx.MsgTx()

	if len(msgTx.TxOut) == 0 {
		return ruleError(ErrBadDrawSettlementTx, "draw settlement has no outputs")
	}
	markerHeight, ok := extractDrawSettlementMarker(msgTx.TxOut[0].PkScript)
	if !ok {
		return ruleError(ErrBadDrawSettlementTx,
			"draw settlement first output is not a valid FDRS marker")
	}
	if markerHeight != drawHeight {
		return ruleError(ErrBadDrawSettlementTx,
			fmt.Sprintf("draw settlement marker height %d != draw block height %d",
				markerHeight, drawHeight))
	}
	if msgTx.TxOut[0].Value != 0 {
		return ruleError(ErrBadDrawSettlementTx,
			"draw settlement marker output must have zero value")
	}
	if len(msgTx.TxIn) == 0 || !IsPoolDrawSentinelInput(msgTx.TxIn[0]) {
		return ruleError(ErrBadDrawSettlementTx,
			"draw settlement first input must be a pool draw sentinel")
	}

	return validatePoolDrawSettlementTx(msgTx, drawHeight, pd)
}

// ValidateVDFDrawTxs enforces draw settlement consensus rules on the VDF result
// block for drawHeight.  Called from validateVDFSettlement.
//
// Rules enforced:
//   - A draw settlement tx must be at index 1 (immediately after coinbase).
//   - At most one draw settlement tx per block.
//   - Any present settlement tx must pass ValidateDrawSettlementTx.
//   - If pool balance > 0: a settlement tx is mandatory.
func ValidateVDFDrawTxs(block *btcutil.Block, drawHeight uint32, view *UtxoViewpoint, pd PoolDrawData) error {
	txns := block.Transactions()
	count := 0
	for i, tx := range txns {
		if !IsDrawSettlementTx(tx) {
			continue
		}
		count++
		if count > 1 {
			return ruleError(ErrBadDrawSettlementTx,
				"VDF result block contains more than one draw settlement transaction")
		}
		if i != 1 {
			return ruleError(ErrBadDrawSettlementTx,
				fmt.Sprintf("draw settlement tx is at index %d; "+
					"must be at index 1 (immediately after coinbase)", i))
		}
		if err := ValidateDrawSettlementTx(tx, drawHeight, view, pd); err != nil {
			return err
		}
	}

	if pd.Balance > 0 && count == 0 {
		return ruleError(ErrMissingDrawSettlementTx,
			fmt.Sprintf("VDF result block for draw %d has no settlement tx "+
				"but pool balance is %d atoms", drawHeight, pd.Balance))
	}

	return nil
}

// BuildPoolDrawSettlementTx constructs a settlement TX for a draw that used
// pool staking (TxVersionPoolStake / FDRP). The TX has a pool sentinel input
// and mints the winner payment from the consensus-tracked pool balance — no
// per-staker UTXO inputs needed (O(1) settlement).
//
// Output layout:
//
//	[0]  FDRS OP_RETURN marker, zero value
//	[1]  P2WPKH output to winner wallet (value = poolBalance) — present only if jackpot
// BuildPoolDrawSettlementTx constructs a settlement TX using the O(1) stored
// draw result. The prize is minted directly to the winner's P2WPKH address —
// no claim transaction required.
func BuildPoolDrawSettlementTx(drawHeight uint32, pd PoolDrawData) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)

	// Output 0: FDRS marker.
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: MakeDrawSettlementMarker(drawHeight),
	})

	// Output 1: prize minted directly to winner's P2WPKH address (jackpot only).
	if pd.JackpotWon {
		tx.AddTxOut(&wire.TxOut{
			Value:    pd.Balance,
			PkScript: MakeWinnerP2WPKHScript(pd.WinnerWalletHash),
		})
	}

	// Pool sentinel input identifies this as a pool settlement.
	tx.AddTxIn(MakePoolDrawSentinelInput(drawHeight))

	return tx
}


