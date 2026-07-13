# ckpool — FalconDraw patches

This is [ckpool](https://bitbucket.org/ckolivas/ckpool) (GPLv3, see `COPYING`),
vendored here with FalconDraw-specific patches applied directly to the source.
Upstream base: `ckolivas/ckpool` commit `a01fcb3e` ("Microoptimise logger.").

Stock ckpool does not understand FalconDraw's consensus rules and will build
blocks that btcd silently rejects forever once real usage starts. Any node
operator who wants to accept external miner connections (a stratum pool, not
just solo-mining via `btcd`'s own `generate=1`) needs this patched build, not
upstream ckpool.

## Patches

1. **Coinbase witness serialization** (`src/stratifier.c`, `process_block()`).
   Stock ckpool computes the BIP141 witness-commitment *output* correctly but
   never serializes the coinbase transaction itself with the SegWit
   marker/flag/witness-stack it commits to. This is invisible as long as a
   block contains no witness-bearing transactions (coinbase-only blocks), but
   FalconDraw is SegWit-active from block 1 — any real stake or send
   transaction in the block triggers the requirement, and btcd rejects the
   submitted block with `the coinbase transaction has 0 items in its witness
   stack when only one is allowed`. The patch splices in the marker (`0x00`),
   flag (`0x01`), and the required 32-byte-zero witness item around the
   existing legacy-serialized coinbase bytes at submission time only (the
   legacy bytes are still what's hashed for the merkle root / share diff,
   per BIP141 — only the final submitted block needs the witness form).

2. **VDF bounty output** (`src/stratifier.c`, `src/stratifier.h`). FalconDraw
   requires the coinbase of a block that settles a draw (i.e. contains a
   VDFResult transaction, tx version 7) to also pay a bounty to the VDF
   proof's submitter — see `blockchain/validate.go` (`vdfBounty` check) and
   `blockchain/stakescript.go` (`CalcVDFBounty`, `VDFBountyHalvingHeight`) in
   the main FalconDraw source. Stock ckpool has no notion of this. The patch
   detects the VDFResult transaction in the block's transaction set (fixed
   390-byte format, tx version 7), extracts the submitter's wallet hash from
   its fixed byte offset, and adds the required P2WPKH output (100 FDRW
   pre-halving-840000, 50 FDRW after — mirroring `CalcVDFBounty` exactly).

Both bugs are "first real usage" bugs: they only manifest once a block
actually needs to carry a witness-bearing transaction or settle a draw with a
nonzero pool balance. A ckpool that has only ever mined coinbase-only test
blocks will look fine and then silently, permanently stall the chain the
moment real activity starts. If you patch/rebase this against a newer
upstream ckpool, re-verify both of these paths still work before deploying.

## Building

```
cd contrib/ckpool
./autogen.sh   # if configure is not already present
./configure
make -C src
```

Deploy `src/ckpool` per your own systemd unit; see `ckpool.conf` for the
`btcd`-facing RPC config (this repo's `btcd` must be running first).
