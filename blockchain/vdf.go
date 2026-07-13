package blockchain

import (
	"context"
	"crypto/sha256"
	"math/big"
	"sync/atomic"
	"time"

	chainhash "github.com/btcsuite/btcd/chainhash/v2"
)

// VDFParams holds the precomputed modular arithmetic parameters for Sloth VDF.
// All three fields are derived from the prime p at startup; store once and reuse.
type VDFParams struct {
	P    *big.Int // 2048-bit prime p ≡ 3 (mod 4)
	Exp  *big.Int // (p+1)/4 — modular square root exponent
	Half *big.Int // (p-1)/2 — Legendre symbol exponent
}

// NewVDFParams builds VDFParams from a prime p ≡ 3 (mod 4).
func NewVDFParams(p *big.Int) *VDFParams {
	exp := new(big.Int).Add(p, big.NewInt(1))
	exp.Rsh(exp, 2) // (p+1)/4

	half := new(big.Int).Sub(p, big.NewInt(1))
	half.Rsh(half, 1) // (p-1)/2

	return &VDFParams{P: new(big.Int).Set(p), Exp: exp, Half: half}
}

// toQR maps x into a quadratic residue mod p.  Called once per SlothEval at
// entry.  If x is already a QR (Legendre symbol == 1) it is returned as-is;
// otherwise p-x is returned.  Once a QR, every subsequent modular sqrt is also
// a QR, so no per-step check is needed.
func (vp *VDFParams) toQR(x *big.Int) *big.Int {
	if x.Sign() == 0 {
		return big.NewInt(1)
	}
	leg := new(big.Int).Exp(x, vp.Half, vp.P)
	if leg.Cmp(big.NewInt(1)) == 0 {
		return new(big.Int).Set(x)
	}
	return new(big.Int).Sub(vp.P, x)
}

// SlothEval computes T sequential modular square roots of the seed.
// Each step: x ← x^((p+1)/4) mod p.  The seed is mapped to a QR once before
// the loop.  Returns nil if ctx is cancelled before completion.
//
// progress, if non-nil, is updated in 10% increments (0,10,...,90) as the
// computation advances. Checked every iteration — a plain uint64 compare, not
// gated behind the coarser per-1024-step cancellation check — so accuracy
// doesn't degrade for small T. Only the step count is exposed, never the
// intermediate value x — the sequential nature of Sloth means knowing how far
// along the computation is gives no way to resume or shortcut it, so this
// carries no grinding/timing risk.
func SlothEval(ctx context.Context, seed [32]byte, T uint64, vp *VDFParams, progress *atomic.Uint32) *big.Int {
	x := new(big.Int).SetBytes(seed[:])
	x.Mod(x, vp.P)
	x = vp.toQR(x)

	// nextDecile counts 1-9; crossing is tested via cross-multiplication
	// (i*10 >= nextDecile*T) rather than dividing T by 10 up front, so the
	// stored value is always an exact multiple of 10 regardless of whether T
	// is evenly divisible by 10 (a T/10 pre-division would drift to values
	// like 9,19,29 instead of 10,20,30 for most T).
	nextDecile := uint64(1)
	for i := uint64(0); i < T; i++ {
		// Check for cancellation every 1024 steps (~11 ms intervals on reference VPS).
		if i&0x3FF == 0 {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
		}
		// Checked every iteration (not gated behind the 1024-step cancellation
		// check above) so small T values still get accurate decile updates —
		// a plain uint64 compare costs nothing next to a modexp squaring. A
		// "for" (not "if") because for very small T more than one decile
		// boundary can fall within a single iteration.
		for progress != nil && nextDecile <= 9 && i*10 >= nextDecile*T {
			progress.Store(uint32(nextDecile * 10))
			nextDecile++
		}
		x.Exp(x, vp.Exp, vp.P)
	}
	return x
}

// vdfBenchmarkMsPerStep measures real-world wall-clock cost of one Sloth
// modexp step (x.Exp(x, vp.Exp, vp.P) — the exact SlothEval hot-loop
// operation) on this machine, over a fixed wall-clock window. Step cost is
// effectively independent of the starting value of x for a fixed
// modulus/exponent, so an arbitrary seed is fine. Returns 0 if the window
// completes zero iterations (caller must treat 0 as "benchmark failed").
func vdfBenchmarkMsPerStep(vp *VDFParams, window time.Duration) float64 {
	x := big.NewInt(2)
	x.Mod(x, vp.P)
	start := time.Now()
	deadline := start.Add(window)
	iterations := 0
	for time.Now().Before(deadline) {
		x.Exp(x, vp.Exp, vp.P)
		iterations++
	}
	if iterations == 0 {
		return 0
	}
	return float64(time.Since(start).Milliseconds()) / float64(iterations)
}

// SlothVerify checks that y is the Sloth output for seed after T steps.
// Verification runs T squarings (much cheaper than evaluation at large p):
//
//	y^(2^T) mod p  must equal  toQR(seed mod p)
func SlothVerify(seed [32]byte, y *big.Int, T uint64, vp *VDFParams) bool {
	want := new(big.Int).SetBytes(seed[:])
	want.Mod(want, vp.P)
	want = vp.toQR(want)

	result := new(big.Int).Set(y)
	for i := uint64(0); i < T; i++ {
		result.Mul(result, result)
		result.Mod(result, vp.P)
	}
	return result.Cmp(want) == 0
}

// DrawSeed derives the draw entropy by hashing the draw block hash together
// with the 256-byte VDF output.  This replaces the old SHA256d(blockHash ||
// drawHeight) — miners cannot grind this because the VDF output requires ~30
// minutes of sequential work.
func DrawSeed(blockHash chainhash.Hash, vdfOutput *big.Int) [32]byte {
	var buf [32 + 256]byte
	copy(buf[:32], blockHash[:])
	vdfOutput.FillBytes(buf[32:]) // zero-pads output to exactly 256 bytes
	first := sha256.Sum256(buf[:])
	return sha256.Sum256(first[:])
}

// PendingVDFResult holds the result of a completed background VDF computation,
// ready to be included in the next block template.
type PendingVDFResult struct {
	DrawHeight          uint32
	DrawBlockHash       chainhash.Hash
	T                   uint64
	Output              *big.Int
	Seed                [32]byte // DrawSeed(DrawBlockHash, Output)
	ComputeSecs         int64    // actual wall-clock seconds the computation took
	SubmitterWalletHash [20]byte // HASH160 of the node operator's pubkey (bounty recipient)
}
