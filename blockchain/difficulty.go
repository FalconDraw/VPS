// Copyright (c) 2013-2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"math/big"
	"time"

	"github.com/btcsuite/btcd/blockchain/internal/workmath"
	"github.com/btcsuite/btcd/chainhash/v2"
)

// HashToBig converts a chainhash.Hash into a big.Int that can be used to
// perform math comparisons.
func HashToBig(hash *chainhash.Hash) *big.Int {
	return workmath.HashToBig(hash)
}

// CompactToBig converts a compact representation of a whole number N to an
// unsigned 32-bit number.  The representation is similar to IEEE754 floating
// point numbers.
//
// Like IEEE754 floating point, there are three basic components: the sign,
// the exponent, and the mantissa.  They are broken out as follows:
//
// - the most significant 8 bits represent the unsigned base 256 exponent
// - bit 23 (the 24th bit) represents the sign bit
// - the least significant 23 bits represent the mantissa
//
//	-------------------------------------------------
//	|   Exponent     |    Sign    |    Mantissa     |
//	-------------------------------------------------
//	| 8 bits [31-24] | 1 bit [23] | 23 bits [22-00] |
//	-------------------------------------------------
//
// The formula to calculate N is:
//
//	N = (-1^sign) * mantissa * 256^(exponent-3)
//
// This compact form is only used in bitcoin to encode unsigned 256-bit numbers
// which represent difficulty targets, thus there really is not a need for a
// sign bit, but it is implemented here to stay consistent with bitcoind.
func CompactToBig(compact uint32) *big.Int {
	return workmath.CompactToBig(compact)
}

// BigToCompact converts a whole number N to a compact representation using
// an unsigned 32-bit number.  The compact representation only provides 23 bits
// of precision, so values larger than (2^23 - 1) only encode the most
// significant digits of the number.  See CompactToBig for details.
func BigToCompact(n *big.Int) uint32 {
	return workmath.BigToCompact(n)
}

// CalcWork calculates a work value from difficulty bits.  Bitcoin increases
// the difficulty for generating a block by decreasing the value which the
// generated hash must be less than.  This difficulty target is stored in each
// block header using a compact representation as described in the documentation
// for CompactToBig.  The main chain is selected by choosing the chain that has
// the most proof of work (highest difficulty).  Since a lower target difficulty
// value equates to higher actual difficulty, the work value which will be
// accumulated must be the inverse of the difficulty.  Also, in order to avoid
// potential division by zero and really small floating point numbers, the
// result adds 1 to the denominator and multiplies the numerator by 2^256.
func CalcWork(bits uint32) *big.Int {
	return workmath.CalcWork(bits)
}

// calcEasiestDifficulty calculates the easiest possible difficulty that a block
// can have given starting difficulty bits and a duration.  It is mainly used to
// verify that claimed proof of work by a block is sane as compared to a
// known good checkpoint.
func (b *BlockChain) calcEasiestDifficulty(bits uint32, duration time.Duration) uint32 {
	// Convert types used in the calculations below.
	durationVal := int64(duration / time.Second)
	adjustmentFactor := big.NewInt(b.chainParams.RetargetAdjustmentFactor)

	// The test network rules allow minimum difficulty blocks after more
	// than twice the desired amount of time needed to generate a block has
	// elapsed.
	if b.chainParams.ReduceMinDifficulty {
		reductionTime := int64(b.chainParams.MinDiffReductionTime /
			time.Second)
		if durationVal > reductionTime {
			return b.chainParams.PowLimitBits
		}
	}

	// Since easier difficulty equates to higher numbers, the easiest
	// difficulty for a given duration is the largest value possible given
	// the number of retargets for the duration and starting difficulty
	// multiplied by the max adjustment factor.
	newTarget := CompactToBig(bits)
	for durationVal > 0 && newTarget.Cmp(b.chainParams.PowLimit) < 0 {
		newTarget.Mul(newTarget, adjustmentFactor)
		durationVal -= b.maxRetargetTimespan
	}

	// Limit new value to the proof of work limit.
	if newTarget.Cmp(b.chainParams.PowLimit) > 0 {
		newTarget.Set(b.chainParams.PowLimit)
	}

	return BigToCompact(newTarget)
}

// findPrevTestNetDifficulty returns the difficulty of the previous block which
// did not have the special testnet minimum difficulty rule applied.
func findPrevTestNetDifficulty(startNode HeaderCtx, c ChainCtx) uint32 {
	// Search backwards through the chain for the last block without
	// the special rule applied.
	iterNode := startNode
	for iterNode != nil && iterNode.Height()%c.BlocksPerRetarget() != 0 &&
		iterNode.Bits() == c.ChainParams().PowLimitBits {

		iterNode = iterNode.Parent()
	}

	// Return the found difficulty or the minimum difficulty if no
	// appropriate block was found.
	lastBits := c.ChainParams().PowLimitBits
	if iterNode != nil {
		lastBits = iterNode.Bits()
	}
	return lastBits
}

// calcNextRequiredDifficulty calculates the required difficulty for the block
// after the passed previous HeaderCtx based on the difficulty retarget rules.
// This function differs from the exported CalcNextRequiredDifficulty in that
// the exported version uses the current best chain as the previous HeaderCtx
// while this function accepts any block node. This function accepts a ChainCtx
// parameter that gives the necessary difficulty context variables.
func calcNextRequiredDifficulty(lastNode HeaderCtx, newBlockTime time.Time,
	c ChainCtx) (uint32, error) {

	// Emulate the same behavior as Bitcoin Core that for regtest there is
	// no difficulty retargeting.
	if c.ChainParams().PoWNoRetargeting {
		return c.ChainParams().PowLimitBits, nil
	}

	// Genesis block.
	if lastNode == nil {
		return c.ChainParams().PowLimitBits, nil
	}

	// LWMA-3 flag day: once active, this replaces both the legacy
	// BlocksPerRetarget-boundary retarget and the EmergencyAdjustWindow
	// one-way ease-down below with a single continuous per-block retarget
	// that corrects in both directions. See LWMA3ActivationHeight's doc
	// comment in chaincfg/params.go for why this exists.
	//
	// Zero means disabled (same "zero disables it" convention as
	// EmergencyAdjustWindow above), not "active from genesis" — needed so
	// test/dev network Params that don't explicitly set this field keep
	// their existing legacy/ReduceMinDifficulty behavior unchanged.
	if activation := c.ChainParams().LWMA3ActivationHeight; activation != 0 && lastNode.Height()+1 >= activation {
		lwma3Bits, err := calcNextRequiredDifficultyLWMA3(lastNode, LWMA3WindowSize, c)
		if err != nil {
			return 0, err
		}

		// LWMA-3 only ever sees solvetimes of blocks that have actually
		// landed: if real hashrate vanishes entirely, no block can land at
		// the frozen difficulty to feed the window a lower value, so the
		// chain would stall forever with no path back. Layer the one-way,
		// wall-clock-driven stall ease-down on top of the LWMA-3 result
		// (never replacing it) so a candidate block's required difficulty
		// keeps easing the longer the real gap since the parent grows,
		// even with zero blocks landing in between. See applyLWMA3StallEase
		// for why this doesn't reuse EmergencyAdjustWindow's threshold.
		return applyLWMA3StallEase(lwma3Bits, lastNode.Timestamp(),
			newBlockTime.Unix(), c), nil
	}

	// Return the previous block's difficulty requirements if this block
	// is not at a difficulty retarget interval.
	if (lastNode.Height()+1)%c.BlocksPerRetarget() != 0 {
		// For networks that support it, allow special reduction of the
		// required difficulty once too much time has elapsed without
		// mining a block.
		if c.ChainParams().ReduceMinDifficulty {
			// Return minimum difficulty when more than the desired
			// amount of time has elapsed without mining a block.
			reductionTime := int64(c.ChainParams().MinDiffReductionTime /
				time.Second)
			allowMinTime := lastNode.Timestamp() + reductionTime
			if newBlockTime.Unix() > allowMinTime {
				return c.ChainParams().PowLimitBits, nil
			}

			// The block was mined within the desired timeframe, so
			// return the difficulty for the last block which did
			// not have the special minimum difficulty rule applied.
			return findPrevTestNetDifficulty(lastNode, c), nil
		}

		// FalconDraw one-way emergency difficulty adjustment: ease difficulty
		// down by RetargetAdjustmentFactor for every full EmergencyAdjustWindow
		// elapsed since the parent block, compounding, until a block lands.
		// This never adjusts upward — recovery happens only through the
		// normal BlocksPerRetarget cycle below — which is what keeps this
		// safe from the boom-bust oscillation exploit that hit Bitcoin
		// Cash's symmetric 2017 EDA (see EmergencyAdjustWindow doc comment).
		//
		// windowsElapsed is capped at maxEmergencyWindowsPerBlock so a single
		// block can't claim an outsized one-shot crash by setting its
		// timestamp near the MaxTimeOffsetSeconds future-drift limit still
		// allowed by block-timestamp validation. A genuine multi-hour
		// drought still fully recovers: each additional slow block re-runs
		// this same capped step relative to its own (already-eased) parent,
		// so the reduction keeps compounding across the sequence of blocks
		// that land during the drought.
		emergencyActive := lastNode.Height()+1 >= c.ChainParams().EmergencyAdjustActivationHeight
		if window := c.ChainParams().EmergencyAdjustWindow; window > 0 && emergencyActive {
			windowSecs := int64(window / time.Second)
			elapsed := newBlockTime.Unix() - lastNode.Timestamp()
			if elapsed > windowSecs {
				const maxEmergencyWindowsPerBlock = 6
				windowsElapsed := elapsed / windowSecs
				if windowsElapsed > maxEmergencyWindowsPerBlock {
					windowsElapsed = maxEmergencyWindowsPerBlock
				}

				newTarget := CompactToBig(lastNode.Bits())
				adjustmentFactor := big.NewInt(c.ChainParams().RetargetAdjustmentFactor)
				for i := int64(0); i < windowsElapsed; i++ {
					newTarget.Mul(newTarget, adjustmentFactor)
					if newTarget.Cmp(c.ChainParams().PowLimit) >= 0 {
						newTarget.Set(c.ChainParams().PowLimit)
						break
					}
				}
				return BigToCompact(newTarget), nil
			}
		}

		// For the main network (or any unrecognized networks), simply
		// return the previous block's difficulty requirements.
		return lastNode.Bits(), nil
	}

	// Get the block node at the previous retarget (targetTimespan days
	// worth of blocks).
	firstNode := lastNode.RelativeAncestorCtx(c.BlocksPerRetarget() - 1)
	if firstNode == nil {
		return 0, AssertError("unable to obtain previous retarget block")
	}

	// Limit the amount of adjustment that can occur to the previous
	// difficulty.
	actualTimespan := lastNode.Timestamp() - firstNode.Timestamp()
	adjustedTimespan := actualTimespan
	if actualTimespan < c.MinRetargetTimespan() {
		adjustedTimespan = c.MinRetargetTimespan()
	} else if actualTimespan > c.MaxRetargetTimespan() {
		adjustedTimespan = c.MaxRetargetTimespan()
	}

	// Special difficulty rule for Testnet4
	oldTarget := CompactToBig(lastNode.Bits())
	if c.ChainParams().EnforceBIP94 {
		// Here we use the first block of the difficulty period. This way
		// the real difficulty is always preserved in the first block as
		// it is not allowed to use the min-difficulty exception.
		oldTarget = CompactToBig(firstNode.Bits())
	}

	// Calculate new target difficulty as:
	//  currentDifficulty * (adjustedTimespan / targetTimespan)
	// The result uses integer division which means it will be slightly
	// rounded down.  Bitcoind also uses integer division to calculate this
	// result.
	newTarget := new(big.Int).Mul(oldTarget, big.NewInt(adjustedTimespan))
	targetTimeSpan := int64(c.ChainParams().TargetTimespan / time.Second)
	newTarget.Div(newTarget, big.NewInt(targetTimeSpan))

	// Limit new value to the proof of work limit.
	if newTarget.Cmp(c.ChainParams().PowLimit) > 0 {
		newTarget.Set(c.ChainParams().PowLimit)
	}

	// Log new target difficulty and return it.  The new target logging is
	// intentionally converting the bits back to a number instead of using
	// newTarget since conversion to the compact representation loses
	// precision.
	newTargetBits := BigToCompact(newTarget)
	log.Debugf("Difficulty retarget at block height %d", lastNode.Height()+1)
	log.Debugf("Old target %08x (%064x)", lastNode.Bits(), oldTarget)
	log.Debugf("New target %08x (%064x)", newTargetBits, CompactToBig(newTargetBits))
	log.Debugf("Actual timespan %v, adjusted timespan %v, target timespan %v",
		time.Duration(actualTimespan)*time.Second,
		time.Duration(adjustedTimespan)*time.Second,
		c.ChainParams().TargetTimespan)

	return newTargetBits, nil
}

// LWMA3WindowSize is the number of blocks averaged by the LWMA-3 retarget
// (roughly 1.67 draw cycles at the 36-block DrawInterval, ~10hr at the
// 10-minute target block time). Chosen over a smaller window (e.g. 45) for
// the extra manipulation-resistance more samples give a weighted average,
// and over a larger one (e.g. 90) so difficulty still reflects a real
// hashrate crash within about half a day.
//
// This is the permanent replacement for the EmergencyAdjustWindow one-way
// ease-down (see its doc comment above). It is wired into
// calcNextRequiredDifficulty behind the chaincfg.Params.LWMA3ActivationHeight
// flag day — see that field's doc comment for the activation height and
// rationale.
const LWMA3WindowSize int32 = 60

// LWMA3SolveTimeClampMultiplier bounds how large a single block's solvetime
// can weigh into the LWMA-3 average (see calcNextRequiredDifficultyLWMA3) —
// individual solvetimes are clamped to +/- this many multiples of
// TargetTimePerBlock before being weighted. applyLWMA3StallEase reuses this
// same multiple as the point past which a real gap since the parent block
// stops being something LWMA-3's own math can represent (its clamp already
// treats any single block taking up to this long as unremarkable), so the
// two mechanisms hand off at a consistent boundary instead of one
// second-guessing the other over normal variance.
const LWMA3SolveTimeClampMultiplier = 6

// applyLWMA3StallEase layers a one-way, wall-clock-driven difficulty
// ease-down on top of an LWMA-3-computed target. LWMA-3 (baseBits here) is
// purely a function of already-landed blocks' timestamps — if hashrate
// drops enough that no block can be found at the current target, no new
// block ever arrives to feed the window a lower value, and the chain stalls
// with no self-recovery path. This function closes that gap: it looks at
// real elapsed time since the parent (newBlockTimestamp is "now" for a
// pending getblocktemplate candidate, or the actual timestamp for a block
// being validated), and once that gap exceeds LWMA3SolveTimeClampMultiplier
// * TargetTimePerBlock — the point beyond which LWMA-3's own solvetime
// clamp can no longer represent how late things really are — eases the
// target down by RetargetAdjustmentFactor per additional full multiple of
// that same window, compounding, exactly like the pre-LWMA-3
// EmergencyAdjustWindow mechanism (which stays gated off post-activation;
// its 1x-TargetTimePerBlock threshold is too tight to layer on top of a
// per-block retarget without constantly fighting ordinary variance — see
// its own doc comment).
//
// Because elapsed here is bounded only by real wall-clock time (a
// dishonest miner can pull a single block's timestamp forward by at most
// MaxTimeOffsetSeconds, not fabricate hours of elapsed history), no
// artificial per-call cap on the number of windows is needed the way the
// legacy mechanism capped windows-per-block: the natural PowLimit ceiling
// below is the only bound required.
func applyLWMA3StallEase(baseBits uint32, parentTimestamp, newBlockTimestamp int64,
	c ChainCtx) uint32 {

	targetTimePerBlock := int64(c.ChainParams().TargetTimePerBlock / time.Second)
	windowSecs := LWMA3SolveTimeClampMultiplier * targetTimePerBlock

	elapsed := newBlockTimestamp - parentTimestamp
	if elapsed <= windowSecs {
		return baseBits
	}

	windowsElapsed := elapsed / windowSecs
	newTarget := CompactToBig(baseBits)
	adjustmentFactor := big.NewInt(c.ChainParams().RetargetAdjustmentFactor)
	powLimit := c.ChainParams().PowLimit
	for i := int64(0); i < windowsElapsed; i++ {
		newTarget.Mul(newTarget, adjustmentFactor)
		if newTarget.Cmp(powLimit) >= 0 {
			newTarget.Set(powLimit)
			break
		}
	}
	return BigToCompact(newTarget)
}

// calcNextRequiredDifficultyLWMA3 computes the next required difficulty
// using LWMA-3 (Linearly Weighted Moving Average), a continuous retarget
// that recalculates every block instead of only at BlocksPerRetarget
// boundaries. Unlike the plain LWMA-1 algorithm, LWMA-3 clamps each
// individual solvetime to +/-6*targetTimePerBlock before applying it, which
// is what defeats the LWMA-1 "jump attack": without the clamp, a miner can
// manufacture one wildly-off timestamp (limited only by the node's future/
// past timestamp acceptance rules) and, because that single solvetime still
// carries its full linear weight, drag the weighted average — and therefore
// the next target — far more than a real hashrate change would justify.
// Clamping caps how much leverage any single block's timestamp can have,
// regardless of how extreme it is set.
//
// windowSize is the number of most-recent blocks to average (production
// value: LWMA3WindowSize). It is a parameter rather than hardcoded so tests
// can exercise the algorithm, including the adversarial jump-attack case,
// without needing to build a window-size-60 chain.
//
// All arithmetic is integer/big.Int — no floating point is used anywhere in
// this calculation, which is a hard requirement for consensus code (see
// project memory on why LWMA-3 was chosen over ASERT).
func calcNextRequiredDifficultyLWMA3(lastNode HeaderCtx, windowSize int32,
	c ChainCtx) (uint32, error) {

	params := c.ChainParams()

	if params.PoWNoRetargeting {
		return params.PowLimitBits, nil
	}
	if lastNode == nil {
		return params.PowLimitBits, nil
	}

	// Not enough history yet to fill a full window: this needs windowSize+1
	// nodes (windowSize solvetimes anchored on one extra parent), so fall
	// back to the immediately preceding difficulty until the chain is long
	// enough.
	if lastNode.Height() < windowSize {
		return lastNode.Bits(), nil
	}

	targetTimePerBlock := int64(params.TargetTimePerBlock / time.Second)
	maxSolvetime := LWMA3SolveTimeClampMultiplier * targetTimePerBlock
	minSolvetime := -maxSolvetime

	weightedSolvetimeSum := big.NewInt(0)
	sumTarget := new(big.Int)

	// Walk the window oldest -> newest. The node at distance d from
	// lastNode (d = windowSize downto 0) is: the anchor block (d ==
	// windowSize, used only for its timestamp to seed the first solvetime)
	// or the j-th block in the window (j = windowSize-d, 1 = oldest average
	// sample, windowSize = lastNode itself, which gets the heaviest weight).
	prevTimestamp := lastNode.RelativeAncestorCtx(windowSize).Timestamp()
	for d := windowSize - 1; d >= 0; d-- {
		node := lastNode.RelativeAncestorCtx(d)
		if node == nil {
			return 0, AssertError("unable to obtain ancestor for LWMA-3 window")
		}

		j := windowSize - d
		solvetime := node.Timestamp() - prevTimestamp
		if solvetime > maxSolvetime {
			solvetime = maxSolvetime
		} else if solvetime < minSolvetime {
			solvetime = minSolvetime
		}
		prevTimestamp = node.Timestamp()

		weightedSolvetimeSum.Add(weightedSolvetimeSum,
			big.NewInt(solvetime*int64(j)))
		sumTarget.Add(sumTarget, CompactToBig(node.Bits()))
	}

	avgTarget := new(big.Int).Div(sumTarget, big.NewInt(int64(windowSize)))

	// k = N(N+1)/2 * T normalizes the weighted solvetime sum: a window
	// where every block landed exactly on target leaves the average target
	// unchanged (weightedSolvetimeSum == k).
	k := big.NewInt(int64(windowSize) * int64(windowSize+1) / 2 * targetTimePerBlock)

	// Defensive floor only — not a policy choice. Even with the per-block
	// clamp above, a pathological run of maximally-negative solvetimes can
	// still drive the weighted sum to zero or below; without this floor
	// that would make nextTarget non-positive, which is not a valid target
	// and would break BigToCompact.
	if weightedSolvetimeSum.Sign() < 1 {
		weightedSolvetimeSum = big.NewInt(1)
	}

	nextTarget := new(big.Int).Mul(avgTarget, weightedSolvetimeSum)
	nextTarget.Div(nextTarget, k)

	if nextTarget.Sign() <= 0 || nextTarget.Cmp(params.PowLimit) > 0 {
		nextTarget.Set(params.PowLimit)
	}

	return BigToCompact(nextTarget), nil
}

// CalcNextRequiredDifficulty calculates the required difficulty for the block
// after the end of the current best chain based on the difficulty retarget
// rules.
//
// This function is safe for concurrent access.
func (b *BlockChain) CalcNextRequiredDifficulty(timestamp time.Time) (uint32, error) {
	b.chainLock.Lock()
	difficulty, err := calcNextRequiredDifficulty(b.bestChain.Tip(), timestamp, b)
	b.chainLock.Unlock()
	return difficulty, err
}
