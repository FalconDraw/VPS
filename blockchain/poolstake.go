package blockchain

import (
	"encoding/binary"
	"fmt"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	chainhash "github.com/btcsuite/btcd/chainhash/v2"
	wire "github.com/btcsuite/btcd/wire/v2"
)

// Pool staking OP_RETURN output script layout (34 bytes total):
//
//	[0]     0x6a — OP_RETURN
//	[1]     0x20 — push 32 bytes
//	[2:22]  20-byte wallet address hash (public — identifies staker for winner announcement)
//	[22:30] 8-byte stake amount in atoms, big-endian int64
//	[30:34] 4-byte draw block height, big-endian uint32
//
// One v6 TX per player. The staked FDRW (= amount atoms) burns from inputs;
// node credits `amount` to the pool balance. No explicit pool UTXO is created.
//
// Winner identified from public walletHash after draw.
// Settlement mints the prize coinbase-style — O(1) regardless of staker count.
// Entropy is SHA256d(blockHash || drawHeight_be4) — VDF replaces this in a future upgrade.
const (
	PoolStakingPayloadSize = 32 // wallet(20)+amount(8)+drawHeight(4)
	PoolStakingScriptSize  = 34 // OP_RETURN(1) + push32(1) + 32
)

// PoolStakingMagic is retained for the pool draw sentinel outpoint prefix.
var PoolStakingMagic = [4]byte{'F', 'D', 'R', 'P'}

// IsPoolStakingOpReturn returns true if script is a valid pool staking OP_RETURN.
func IsPoolStakingOpReturn(script []byte) bool {
	return len(script) == PoolStakingScriptSize &&
		script[0] == 0x6a &&
		script[1] == 0x20
}

// MakePoolStakingScript builds the 34-byte OP_RETURN script for a pool staking output.
func MakePoolStakingScript(walletHash [20]byte, amount int64, drawHeight uint32) []byte {
	script := make([]byte, PoolStakingScriptSize)
	script[0] = 0x6a // OP_RETURN
	script[1] = 0x20 // push 32 bytes
	copy(script[2:22], walletHash[:])
	binary.BigEndian.PutUint64(script[22:30], uint64(amount))
	binary.BigEndian.PutUint32(script[30:34], drawHeight)
	return script
}

// ExtractPoolStakingData parses a pool staking OP_RETURN script.
func ExtractPoolStakingData(script []byte) (walletHash [20]byte, amount int64, drawHeight uint32, err error) {
	if !IsPoolStakingOpReturn(script) {
		return walletHash, 0, 0, fmt.Errorf("not a pool staking OP_RETURN")
	}
	copy(walletHash[:], script[2:22])
	amount = int64(binary.BigEndian.Uint64(script[22:30]))
	drawHeight = binary.BigEndian.Uint32(script[30:34])
	return walletHash, amount, drawHeight, nil
}

// FindPoolStakingOpReturn returns the index and script of the FDRP OP_RETURN
// output in msgTx, or (-1, nil) if absent.
func FindPoolStakingOpReturn(msgTx *wire.MsgTx) (int, []byte) {
	for i, out := range msgTx.TxOut {
		if IsPoolStakingOpReturn(out.PkScript) {
			return i, out.PkScript
		}
	}
	return -1, nil
}

// IsPoolStakingTx returns true if tx is a TxVersionPoolStake transaction
// containing a valid FDRP OP_RETURN output.
func IsPoolStakingTx(tx *btcutil.Tx) bool {
	if tx.MsgTx().Version != wire.TxVersionPoolStake {
		return false
	}
	idx, _ := FindPoolStakingOpReturn(tx.MsgTx())
	return idx >= 0
}

// ValidatePoolStakeTx checks that a transaction is a well-formed pool staking
// transaction. Enforces:
//   - TxVersionPoolStake version
//   - No coinbase inputs
//   - Exactly one FDRP OP_RETURN output with zero value
//   - drawHeight is a valid future draw block
//   - expiry in [1, MaxStakingExpiry]
//   - tokenCount >= 1
//   - poolContribution = tokenCount × SatoshiPerBitcoin
//   - inputValue >= poolContribution + changeValue (non-negative fee)
func ValidatePoolStakeTx(tx *btcutil.Tx, blockHeight int32, utxoView *UtxoViewpoint) error {
	msgTx := tx.MsgTx()

	if msgTx.Version != wire.TxVersionPoolStake {
		return ruleError(ErrBadTxInput,
			fmt.Sprintf("pool staking tx must use version %d, got %d",
				wire.TxVersionPoolStake, msgTx.Version))
	}

	for _, txIn := range msgTx.TxIn {
		if txIn.PreviousOutPoint.Index == 0xffffffff {
			return ruleError(ErrBadTxInput, "pool staking tx must not have coinbase inputs")
		}
		if IsPoolDrawSentinelInput(txIn) {
			continue
		}
		// Pool staking inputs carry the Schnorr sig in the witness field (SegWit
		// discount); scriptSig must be empty.
		if len(txIn.SignatureScript) != 0 {
			return ruleError(ErrBadTxInput, "pool staking inputs must have empty scriptSig (use witness)")
		}
		if len(txIn.Witness) != 2 || len(txIn.Witness[0]) != 64 || len(txIn.Witness[1]) != 33 {
			return ruleError(ErrBadTxInput, "pool staking input witness must be [sig(64), pubkey(33)]")
		}
	}

	opReturnIdx := -1
	var walletHash [20]byte
	var amount int64
	var drawHeight uint32
	for i, txOut := range msgTx.TxOut {
		if IsPoolStakingOpReturn(txOut.PkScript) {
			if opReturnIdx >= 0 {
				return ruleError(ErrBadTxInput, "pool staking tx has more than one FDRP OP_RETURN")
			}
			if txOut.Value != 0 {
				return ruleError(ErrBadTxInput, "pool staking OP_RETURN output must have zero value")
			}
			var err error
			walletHash, amount, drawHeight, err = ExtractPoolStakingData(txOut.PkScript)
			if err != nil {
				return ruleError(ErrBadTxInput, fmt.Sprintf("pool staking output %d: %v", i, err))
			}
			opReturnIdx = i
		}
	}
	if opReturnIdx < 0 {
		return ruleError(ErrBadTxInput, "pool staking tx has no FDRP OP_RETURN output")
	}

	if walletHash == ([20]byte{}) {
		return ruleError(ErrBadTxInput, "pool staking: wallet hash must not be zero (reserved)")
	}
	if !IsDrawBlock(int32(drawHeight)) {
		return ruleError(ErrBadTxInput,
			fmt.Sprintf("pool staking: drawHeight %d is not a draw block", drawHeight))
	}
	if amount < btcutil.SatoshiPerBitcoin {
		return ruleError(ErrBadTxInput,
			fmt.Sprintf("pool staking: minimum stake is 1 FDRW (%g atoms); got %d",
				btcutil.SatoshiPerBitcoin, amount))
	}
	if amount%btcutil.SatoshiPerBitcoin != 0 {
		return ruleError(ErrBadTxInput,
			fmt.Sprintf("pool staking: stake must be a whole number of FDRW; %d atoms is not divisible by %g",
				amount, btcutil.SatoshiPerBitcoin))
	}

	if utxoView != nil {
		var inputValue int64
		for _, txIn := range msgTx.TxIn {
			utxo := utxoView.LookupEntry(txIn.PreviousOutPoint)
			if utxo == nil {
				return ruleError(ErrMissingTxOut,
					fmt.Sprintf("pool staking input references missing utxo %v", txIn.PreviousOutPoint))
			}
			inputValue += utxo.Amount()
		}
		var changeValue int64
		for i, txOut := range msgTx.TxOut {
			if i == opReturnIdx {
				continue
			}
			changeValue += txOut.Value
		}
		if inputValue < amount+changeValue {
			return ruleError(ErrBadTxInput,
				fmt.Sprintf("pool staking: inputs (%d atoms) < declared stake (%d) + change (%d); fee would be negative",
					inputValue, amount, changeValue))
		}
	}

	return nil
}

// PoolStakingSchnorrWitness builds the 2-item witness stack for a pool staking
// TX input: [sig[64], pubkey[33]]. The input scriptSig must be empty; the
// witness data receives the 4× SegWit weight discount.
func PoolStakingSchnorrWitness(privKey *btcec.PrivateKey, sigHash []byte) ([][]byte, error) {
	sig, err := schnorr.Sign(privKey, sigHash)
	if err != nil {
		return nil, fmt.Errorf("pool staking schnorr sign: %w", err)
	}
	return [][]byte{sig.Serialize(), privKey.PubKey().SerializeCompressed()}, nil
}

// VerifyPoolStakingSchnorrWitness validates the witness on a pool staking input.
// Expected: witness[0] = 64-byte Schnorr sig, witness[1] = 33-byte compressed pubkey.
func VerifyPoolStakingSchnorrWitness(witness wire.TxWitness, sigHash []byte) (*btcec.PublicKey, error) {
	if len(witness) != 2 {
		return nil, fmt.Errorf("pool staking witness: want 2 items, got %d", len(witness))
	}
	if len(witness[0]) != 64 {
		return nil, fmt.Errorf("pool staking witness[0]: want 64-byte sig, got %d", len(witness[0]))
	}
	if len(witness[1]) != 33 {
		return nil, fmt.Errorf("pool staking witness[1]: want 33-byte pubkey, got %d", len(witness[1]))
	}
	sig, err := schnorr.ParseSignature(witness[0])
	if err != nil {
		return nil, fmt.Errorf("pool staking witness: parse sig: %w", err)
	}
	pubKey, err := btcec.ParsePubKey(witness[1])
	if err != nil {
		return nil, fmt.Errorf("pool staking witness: parse pubkey: %w", err)
	}
	if !sig.Verify(sigHash, pubKey) {
		return nil, fmt.Errorf("pool staking witness: signature verification failed")
	}
	return pubKey, nil
}

// ─── Pool draw sentinel ───────────────────────────────────────────────────────
//
// The settlement TX for a pool-staking draw uses a sentinel input to convey
// the draw height, replacing the per-staker UTXO inputs used in legacy
// settlement TXs. The sentinel outpoint has "FDRP" as the first 4 bytes of
// its hash and the draw height at bytes 4:8 (big-endian); the rest is zero.
// It will not collide with real UTXO hashes (SHA256d output is uniform).

// PoolDrawSentinelOutpoint returns the sentinel outpoint for a pool settlement.
func PoolDrawSentinelOutpoint(drawHeight uint32) wire.OutPoint {
	var h chainhash.Hash
	copy(h[:4], PoolStakingMagic[:]) // "FDRP"
	binary.BigEndian.PutUint32(h[4:8], drawHeight)
	return wire.OutPoint{Hash: h, Index: 0}
}

// IsPoolDrawSentinelInput returns true if txIn is a pool draw sentinel.
func IsPoolDrawSentinelInput(txIn *wire.TxIn) bool {
	h := txIn.PreviousOutPoint.Hash
	return h[0] == PoolStakingMagic[0] &&
		h[1] == PoolStakingMagic[1] &&
		h[2] == PoolStakingMagic[2] &&
		h[3] == PoolStakingMagic[3] &&
		txIn.PreviousOutPoint.Index == 0
}

// PoolSentinelDrawHeight extracts the draw height from a pool sentinel input.
func PoolSentinelDrawHeight(txIn *wire.TxIn) uint32 {
	return binary.BigEndian.Uint32(txIn.PreviousOutPoint.Hash[4:8])
}

// MakePoolDrawSentinelInput builds the sentinel TxIn for a pool settlement TX.
// ScriptSig carries the draw height as a 4-byte push for transparency.
func MakePoolDrawSentinelInput(drawHeight uint32) *wire.TxIn {
	var scriptSig [5]byte
	scriptSig[0] = 0x04 // push 4 bytes
	binary.BigEndian.PutUint32(scriptSig[1:5], drawHeight)
	return &wire.TxIn{
		PreviousOutPoint: PoolDrawSentinelOutpoint(drawHeight),
		SignatureScript:  scriptSig[:],
		Sequence:         wire.MaxTxInSequenceNum,
	}
}
