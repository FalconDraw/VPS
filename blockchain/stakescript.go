package blockchain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/big"

	"github.com/btcsuite/btcd/btcutil/v2"
	fdaddress "github.com/btcsuite/btcd/address/v2"
	wire "github.com/btcsuite/btcd/wire/v2"
)

// VDFResult OP_RETURN layout (328-byte pkScript):
//
//	[0]      0x6a  — OP_RETURN
//	[1]      0x4d  — OP_PUSHDATA2
//	[2:4]    0x44 0x01  — 324 bytes, little-endian
//	[4:8]    "FDRV" magic
//	[8:12]   drawHeight, big-endian uint32
//	[12:44]  drawBlockHash, 32 bytes (the hash of draw block N, VDF input seed)
//	[44:52]  T_used, big-endian uint64
//	[52:308] vdf_output, 256 bytes big-endian (FillBytes zero-padded)
//	[308:328] submitter wallet hash, 20 bytes (HASH160 of submitter pubkey)
const (
	vdfResultMagicOffset      = 4
	vdfResultHeightOffset     = 8
	vdfResultBlockHashOffset  = 12
	vdfResultTOffset          = 44
	vdfResultOutputOffset     = 52
	vdfResultSubmitterOffset  = 308
	vdfResultPayloadSize      = 324 // magic(4)+height(4)+blockhash(32)+T(8)+output(256)+submitter(20)
	vdfResultScriptSize       = 328 // OP_RETURN(1)+OP_PUSHDATA2(1)+len(2)+payload(324)

	// VDFBountyHalvingHeight is the block height at which the VDF bounty halves.
	// Aligns with halving 4 (height 840000) when the block reward first drops
	// below 100 FDRW (to 62). Before this height: 100 FDRW. At or after: 50 FDRW.
	VDFBountyHalvingHeight = int32(840000)
)

var vdfResultMagic = [4]byte{'F', 'D', 'R', 'V'}

// CalcVDFBounty returns the VDF bounty in atoms for the given block height.
// 100 FDRW before the halving height, 50 FDRW at or after (permanent floor).
func CalcVDFBounty(height int32) int64 {
	if height < VDFBountyHalvingHeight {
		return 100 * btcutil.SatoshiPerBitcoin
	}
	return 50 * btcutil.SatoshiPerBitcoin
}

// BuildVDFResultOpReturn constructs the 328-byte pkScript for a VDFResult output.
func BuildVDFResultOpReturn(drawHeight uint32, drawBlockHash [32]byte, T uint64, vdfOutput *big.Int, submitterWalletHash [20]byte) []byte {
	script := make([]byte, vdfResultScriptSize)
	script[0] = 0x6a // OP_RETURN
	script[1] = 0x4d // OP_PUSHDATA2
	// payload length 324 in little-endian
	script[2] = byte(vdfResultPayloadSize & 0xff)
	script[3] = byte(vdfResultPayloadSize >> 8)
	copy(script[vdfResultMagicOffset:], vdfResultMagic[:])
	binary.BigEndian.PutUint32(script[vdfResultHeightOffset:], drawHeight)
	copy(script[vdfResultBlockHashOffset:], drawBlockHash[:])
	binary.BigEndian.PutUint64(script[vdfResultTOffset:], T)
	vdfOutput.FillBytes(script[vdfResultOutputOffset : vdfResultOutputOffset+256]) // zero-pads to 256 bytes
	copy(script[vdfResultSubmitterOffset:], submitterWalletHash[:])
	return script
}

// IsVDFResultTx returns true if tx is a Sloth VDF result transaction (version 7).
func IsVDFResultTx(tx *btcutil.Tx) bool {
	return tx.MsgTx().Version == wire.TxVersionVDFResult
}

// ExtractVDFResultData parses the VDFResult OP_RETURN and returns its fields.
func ExtractVDFResultData(tx *btcutil.Tx) (drawHeight uint32, drawBlockHash [32]byte, T uint64, vdfOutput *big.Int, submitterWalletHash [20]byte, err error) {
	outs := tx.MsgTx().TxOut
	if len(outs) == 0 {
		return 0, drawBlockHash, 0, nil, submitterWalletHash, errors.New("VDFResult tx has no outputs")
	}
	script := outs[0].PkScript
	if len(script) != vdfResultScriptSize {
		return 0, drawBlockHash, 0, nil, submitterWalletHash, errors.New("VDFResult script wrong size")
	}
	if script[0] != 0x6a || script[1] != 0x4d {
		return 0, drawBlockHash, 0, nil, submitterWalletHash, errors.New("VDFResult script missing OP_RETURN/OP_PUSHDATA2")
	}
	if script[vdfResultMagicOffset] != vdfResultMagic[0] ||
		script[vdfResultMagicOffset+1] != vdfResultMagic[1] ||
		script[vdfResultMagicOffset+2] != vdfResultMagic[2] ||
		script[vdfResultMagicOffset+3] != vdfResultMagic[3] {
		return 0, drawBlockHash, 0, nil, submitterWalletHash, errors.New("VDFResult missing FDRV magic")
	}
	drawHeight = binary.BigEndian.Uint32(script[vdfResultHeightOffset:])
	copy(drawBlockHash[:], script[vdfResultBlockHashOffset:vdfResultTOffset])
	T = binary.BigEndian.Uint64(script[vdfResultTOffset:])
	vdfOutput = new(big.Int).SetBytes(script[vdfResultOutputOffset:vdfResultSubmitterOffset])
	copy(submitterWalletHash[:], script[vdfResultSubmitterOffset:])
	return drawHeight, drawBlockHash, T, vdfOutput, submitterWalletHash, nil
}

// BuildVDFResultTx constructs a TxVersionVDFResult transaction.  It has a
// sentinel input with 'FDRV' magic in the hash (non-zero) so that IsCoinBaseTx
// does not match it (coinbase requires zero hash + MaxPrevOutIndex).
func BuildVDFResultTx(drawHeight uint32, drawBlockHash [32]byte, T uint64, vdfOutput *big.Int, submitterWalletHash [20]byte) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersionVDFResult)
	var sentinelHash [32]byte
	copy(sentinelHash[:4], vdfResultMagic[:]) // 'FDRV' — non-zero, not coinbase
	binary.BigEndian.PutUint32(sentinelHash[4:8], drawHeight)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: sentinelHash, Index: 0},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: BuildVDFResultOpReturn(drawHeight, drawBlockHash, T, vdfOutput, submitterWalletHash),
	})
	return tx
}

const (
	// DrawInterval is the number of blocks between Falcon Draws.
	// At 10-minute block time this is ~6 hours per draw, 4 draws per day.
	DrawInterval = int32(36)

	// MaxStakesPerWalletPerDraw is the maximum number of pool stake
	// transactions accepted from a single wallet hash per draw height.
	// Enforced in both the mempool and the staking index to prevent a
	// single actor from consuming all block space with tiny stakes.
	MaxStakesPerWalletPerDraw = 5
)

// JackpotPoolWalletHash is the reserved all-zeros wallet hash used as the
// jackpot accumulation sentinel. In pool staking (v6), no real staker may
// use this hash — it is rejected by ValidatePoolStakeTx.
var JackpotPoolWalletHash = [20]byte{}

// IsDrawBlock returns true if blockHeight is a Falcon Draw trigger block
// (H mod DrawInterval == 0, and H > 0).
func IsDrawBlock(blockHeight int32) bool {
	return blockHeight > 0 && blockHeight%DrawInterval == 0
}


// NextDrawHeight returns the height of the next draw block strictly after
// currentHeight.
func NextDrawHeight(currentHeight int32) int32 {
	next := currentHeight + 1
	rem := next % DrawInterval
	if rem == 0 {
		return next
	}
	return next + (DrawInterval - rem)
}

// StakedTokenCount returns the number of whole FDRW tokens in a pool stake amount.
func StakedTokenCount(atoms int64) int64 {
	return atoms / btcutil.SatoshiPerBitcoin
}

// StakingTxSigHash returns the hash signed by pool staking transaction inputs:
// SHA256d(legacy serialization with all scriptSigs and witness blanked).
func StakingTxSigHash(msgTx *wire.MsgTx) []byte {
	blanked := msgTx.Copy()
	for _, in := range blanked.TxIn {
		in.SignatureScript = nil
		in.Witness = nil
	}
	h1 := sha256.Sum256(serializeTx(blanked))
	buf := sha256.Sum256(h1[:])
	return buf[:]
}

// serializeTx serializes a MsgTx to bytes for hashing purposes.
func serializeTx(tx *wire.MsgTx) []byte {
	var w countWriter
	_ = tx.Serialize(&w)
	result := make([]byte, w.n)
	sw := &sliceWriter{buf: result}
	_ = tx.Serialize(sw)
	return result
}

type countWriter struct{ n int }

func (c *countWriter) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

type sliceWriter struct {
	buf []byte
	pos int
}

func (s *sliceWriter) Write(p []byte) (int, error) {
	n := copy(s.buf[s.pos:], p)
	s.pos += n
	return n, nil
}


// pubKeyHash returns Hash160 of a compressed public key.
func pubKeyHash(compressedPubKey []byte) [20]byte {
	h := fdaddress.Hash160(compressedPubKey)
	var out [20]byte
	copy(out[:], h)
	return out
}
