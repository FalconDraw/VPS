package blockchain

import (
	chainhash "github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// BlockPoWHash returns the SHA256d proof-of-work hash of the block header.
// With SHA256d, the PoW hash is identical to the block identity hash.
func BlockPoWHash(header *wire.BlockHeader) chainhash.Hash {
	return header.BlockHash()
}
