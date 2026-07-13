package main

import (
	"bytes"
	"encoding/hex"
	"fmt"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	wire "github.com/btcsuite/btcd/wire/v2"
)

const (
	feePerByte   = int64(100) // atoms per byte
	dustThreshold = int64(1000) // minimum change output to include
)

// serializeTx hex-encodes a wire transaction for RPC broadcast.
func serializeTx(tx *wire.MsgTx) (string, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf.Bytes()), nil
}

// p2wpkhScript builds a native P2WPKH locking script: OP_0 <hash20> (22 bytes).
func p2wpkhScript(pubKeyHash []byte) []byte {
	script := make([]byte, 22)
	script[0] = 0x00 // OP_0 (witness version 0)
	script[1] = 0x14 // push 20 bytes
	copy(script[2:22], pubKeyHash)
	return script
}

// selectUTXOs picks UTXOs from the given list sufficient to cover targetAtoms
// plus an estimated fee based on the estimated tx size. Returns selected UTXOs
// and the change amount (may be 0 if below dustThreshold). Returns an error
// when funds are insufficient.
func selectUTXOs(utxos []addrUTXO, targetAtoms int64, estimatedTxBytes int) ([]addrUTXO, int64, error) {
	fee := feePerByte * int64(estimatedTxBytes)
	need := targetAtoms + fee

	// Prefer UTXOs with at least 1 confirmation; fall back to unconfirmed if needed.
	var confirmed, unconfirmed []addrUTXO
	for _, u := range utxos {
		if u.IsStaking {
			continue // staking UTXOs are locked
		}
		if u.IsCoinbase && u.Confirmations < 100 {
			continue // immature coinbase
		}
		if u.Confirmations >= 1 {
			confirmed = append(confirmed, u)
		} else {
			unconfirmed = append(unconfirmed, u)
		}
	}
	candidates := append(confirmed, unconfirmed...)

	var selected []addrUTXO
	var total int64
	for _, u := range candidates {
		selected = append(selected, u)
		total += u.Atoms
		if total >= need {
			break
		}
	}
	if total < need {
		return nil, 0, fmt.Errorf("insufficient funds: have %.8f FDRW, need %.8f FDRW (incl. fee)",
			float64(total)/1e8, float64(need)/1e8)
	}
	change := total - targetAtoms - fee
	if change < dustThreshold {
		change = 0
	}
	return selected, change, nil
}

// buildPoolStakeTx constructs a TxVersionPoolStake (v6/FDRP) transaction.
// stakeAtoms is the amount to stake (must be whole FDRW, i.e. divisible by SatoshiPerBitcoin).
// Schnorr sigs go in the witness field (SegWit discount); scriptSig is empty.
func buildPoolStakeTx(privKey *btcec.PrivateKey, walletHash [20]byte, changeScript []byte,
	fundingUTXOs []addrUTXO, stakeAtoms int64, drawHeight uint32,
	change int64) (*wire.MsgTx, error) {

	tx := wire.NewMsgTx(wire.TxVersionPoolStake)

	for _, u := range fundingUTXOs {
		hash, err := chainhash.NewHashFromStr(u.TxID)
		if err != nil {
			return nil, fmt.Errorf("parsing txid %s: %w", u.TxID, err)
		}
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: *hash, Index: u.Vout},
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}

	// Output 0: FDRP OP_RETURN — walletHash(20)+amount(8)+drawHeight(4).
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: blockchain.MakePoolStakingScript(walletHash, stakeAtoms, drawHeight),
	})

	// Output 1 (optional): change back to wallet.
	if change > 0 {
		tx.AddTxOut(&wire.TxOut{Value: change, PkScript: changeScript})
	}

	sigHash := blockchain.StakingTxSigHash(tx)
	for i := range tx.TxIn {
		witness, err := blockchain.PoolStakingSchnorrWitness(privKey, sigHash)
		if err != nil {
			return nil, fmt.Errorf("signing pool stake input %d: %w", i, err)
		}
		tx.TxIn[i].Witness = witness
	}

	return tx, nil
}

// buildTransferTx builds a standard P2WPKH-to-P2WPKH transfer (TxVersion=1).
// Signs each input with BIP143 ECDSA witness [sig, pubkey]; scriptSig is empty.
// estVbytes: 141 for 1-in/2-out; add 69 per additional input.
func buildTransferTx(privKey *btcec.PrivateKey, walletHash [20]byte,
	recipientScript []byte, utxos []addrUTXO, sendAtoms int64) (*wire.MsgTx, error) {

	const (
		baseVbytes     = 73 // 2-output overhead (no inputs)
		perInputVbytes = 69 // per P2WPKH input after witness discount
	)

	// Two-pass: select UTXOs with an estimate, then sign.
	estVbytes := baseVbytes + perInputVbytes
	selected, change, err := selectUTXOs(utxos, sendAtoms, estVbytes)
	if err != nil {
		// Retry with 2-input estimate if we needed more UTXOs.
		estVbytes = baseVbytes + perInputVbytes*2
		selected, change, err = selectUTXOs(utxos, sendAtoms, estVbytes)
		if err != nil {
			return nil, err
		}
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	prevFetcher := txscript.NewMultiPrevOutFetcher(nil)
	p2wpkh := p2wpkhScript(walletHash[:])

	for _, u := range selected {
		hash, err := chainhash.NewHashFromStr(u.TxID)
		if err != nil {
			return nil, fmt.Errorf("parsing txid %s: %w", u.TxID, err)
		}
		op := wire.OutPoint{Hash: *hash, Index: u.Vout}
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: op,
			Sequence:         wire.MaxTxInSequenceNum,
		})
		prevFetcher.AddPrevOut(op, &wire.TxOut{Value: u.Atoms, PkScript: p2wpkh})
	}

	tx.AddTxOut(&wire.TxOut{Value: sendAtoms, PkScript: recipientScript})
	if change > 0 {
		tx.AddTxOut(&wire.TxOut{Value: change, PkScript: p2wpkh})
	}

	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	for i, u := range selected {
		witness, err := txscript.WitnessSignature(
			tx, sigHashes, i, u.Atoms, p2wpkh,
			txscript.SigHashAll, privKey, true,
		)
		if err != nil {
			return nil, fmt.Errorf("signing input %d: %w", i, err)
		}
		tx.TxIn[i].Witness = witness
	}
	return tx, nil
}

// btcAmount formats atoms as a human-readable FDRW amount.
func btcAmount(atoms int64) string {
	return btcutil.Amount(atoms).String()
}
