# FalconDraw Node

FalconDraw is a Bitcoin fork written in Go. It adds one thing: a provably fair on-chain prize draw every 36 blocks (~6 hours), funded entirely by participant stakes. There is no operator, no house fee, and no custodial control — the draw mechanic is enforced by consensus.

- **Ticker:** FDRW
- **Block time:** ~10 min
- **Draw interval:** every 36 blocks
- **Block reward:** `max(1, floor(1000 / 2^(floor(H/210000))))` FDRW, halving every 210,000 blocks
- **Asymptotic supply:** ~430M FDRW
- **Address prefix:** `fdrw` (bech32, SegWit P2WPKH from block 1)
- **PoW:** SHA256d (no RandomX, no CGo)
- **Draw entropy:** Sloth VDF — sequential, verifiable, miner-grinding resistant
- **Genesis hash:** `0000f69ed582eaa7857f7ec4c6864302bcf1cef067c5cdf16d071a675ac48f95`

See the [whitepaper](https://falcondraw.com/whitepaper.pdf) for full protocol specification.

---

## Requirements

[Go](https://golang.org) 1.25 or newer.

```bash
go version   # must be go1.25 or higher
```

---

## Build

```bash
git clone https://github.com/FalconDraw/VPS.git falcondraw
cd falcondraw
go build -o btcd .
```

The resulting `btcd` binary is the full node.

---

## Configuration

Copy the sample config and edit it:

```bash
mkdir -p ~/.btcd
cp sample-btcd.conf ~/.btcd/btcd.conf
```

Minimum required settings in `~/.btcd/btcd.conf`:

```ini
[Application Options]

; RPC credentials — required to enable the RPC server
rpcuser=yourusername
rpcpass=yourpassword

; Bootstrap peer — connect to the mainnet seed node
addpeer=23.238.60.122:8333

; Enable full transaction and address indexes (recommended)
txindex=1
addrindex=1

; Optional: enable CPU mining and set a payout address
; generate=1
; miningaddr=fdrw1q...youraddress...
```

**Important:** never use `--datadir` on the command line. Always use `--configfile`. Passing `--datadir` directly silently starts a fresh chain.

---

## Run

```bash
./btcd --configfile=~/.btcd/btcd.conf
```

Or run as a systemd service (see below).

The node will connect to the seed peer, download the chain, and begin validating. The VDF goroutine starts automatically after each draw block and posts the entropy result once computed (~18–30 min). Mining nodes that include a valid VDF result in a block automatically receive the settlement transaction in the same block template.

**CPU mining vs. VDF:** the VDF computation runs on its own goroutine and competes with the CPU miner for cores. On small VPS instances, mining at your full core count can starve the VDF and delay draw settlement well beyond the expected time. If draws are confirming slowly, set `genthreads` to one less than your core count so the VDF always has a free core.

---

## Systemd Service (Linux)

```ini
[Unit]
Description=FalconDraw Node
After=network.target

[Service]
ExecStart=/path/to/btcd --configfile=/home/youruser/.btcd/btcd.conf
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable falcondraw
sudo systemctl start falcondraw
```

---

## RPC

The RPC server listens on `127.0.0.1:8334` by default (TLS). Use `btcctl` or any JSON-RPC client.

Standard Bitcoin RPC commands work as expected. FalconDraw adds:

| Command | Description |
|---------|-------------|
| `getdrawinfo` | Active draw height, pool balance, VDF phase and progress |
| `getdrawresult <height>` | Result of a settled draw (seed, winner, pool, rollover) |
| `getdrawstakers <height>` | All stakers for a draw with token counts |
| `getdrawtickets <height>` | Draw seed and VDF output for a settled draw |
| `getdrawsizeinfo` | Current mempool stake count and total tokens |
| `getmempoolstakes` | Unconfirmed stakes currently in the mempool |
| `getwalletstakehistory <walletHash>` | Stake history for a wallet |

---

## Draw Mechanic

Every 36 blocks, a draw is triggered. The node:

1. Starts `SlothEval(blockHash, T, prime)` in a background goroutine (~18–30 min)
2. Broadcasts a VDF result transaction (version 7) to the mempool on completion
3. The next block containing that v7 tx **must** also include the settlement transaction (consensus rule)
4. Settlement mints a P2WPKH output to the winner — no winner communication required
5. If no stakers or the position falls outside the stake range, the pool rolls over to the next draw

Win probability is exactly 10% per draw. Expected value is exactly 1 FDRW per token staked (zero house edge).

---

## Network Parameters

| Parameter | Value |
|-----------|-------|
| P2P port | 8333 |
| RPC port | 8334 |
| Magic bytes | `0x414570dd` (FalconNet) |
| DNS seeds | None — use `addpeer` |
| Bootstrap node | `23.238.60.122:8333` |

---

## Genesis Block

| Field | Value |
|-------|-------|
| Height | 0 |
| Hash | `0000f69ed582eaa7857f7ec4c6864302bcf1cef067c5cdf16d071a675ac48f95` |
| Merkle root | `54c447409a78dc3fe132d0579033aacaba3c4ca108e3f78d8df00afb0417a0e6` |
| Timestamp | `2026-05-30 00:00:00 UTC` (unix `1748563200`) |
| Bits | `0x1f0fffff` |
| Nonce | `8634` (`0x000021ba`) |
| Version | 1 |
| Coinbase reward | 1000 FDRW |

A node that connects to a peer on the wrong chain will be rejected immediately — genesis hash mismatch triggers disconnection at the handshake.

---

## License

ISC License. See [LICENSE](LICENSE).
