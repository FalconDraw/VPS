package blockchain

import (
	"math/big"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// buildLWMA3Chain links len(timestamps) blockNodes into a parent chain,
// oldest first, so RelativeAncestorCtx walks a real ancestor chain the way
// calcNextRequiredDifficultyLWMA3 expects. len(timestamps) must equal
// len(bits); the returned node is the last (most recent) one.
func buildLWMA3Chain(t *testing.T, timestamps []int64, bits []uint32) *blockNode {
	t.Helper()
	require.Equal(t, len(timestamps), len(bits))

	var parent *blockNode
	var last *blockNode
	for i, ts := range timestamps {
		header := &wire.BlockHeader{
			Bits:      bits[i],
			Timestamp: time.Unix(ts, 0),
		}
		node := newBlockNode(header, parent)
		last = node
		parent = node
	}
	return last
}

// lwma3TestWindow is a small window used across these tests instead of the
// production LWMA3WindowSize (60) purely so building the ancestor chain
// stays cheap; the algorithm doesn't depend on N being any particular size.
const lwma3TestWindow int32 = 10

// lwma3SteadyTimestamps returns windowSize+1 timestamps starting at t0, each
// exactly targetSecs after the last, i.e. a chain where every block landed
// precisely on target.
func lwma3SteadyTimestamps(t0 int64, windowSize int32, targetSecs int64) []int64 {
	timestamps := make([]int64, windowSize+1)
	timestamps[0] = t0
	for i := int32(1); i <= windowSize; i++ {
		timestamps[i] = timestamps[i-1] + targetSecs
	}
	return timestamps
}

func uniformBits(windowSize int32, bits uint32) []uint32 {
	out := make([]uint32, windowSize+1)
	for i := range out {
		out[i] = bits
	}
	return out
}

// TestLWMA3NotEnoughHistory confirms a chain shorter than the window falls
// back to the parent's unchanged difficulty instead of averaging over a
// partial (and therefore skewed) window.
func TestLWMA3NotEnoughHistory(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}

	targetSecs := int64(params.TargetTimePerBlock / time.Second)
	// One block short of the window.
	timestamps := lwma3SteadyTimestamps(1_000_000, lwma3TestWindow-1, targetSecs)
	bits := uniformBits(lwma3TestWindow-1, params.PowLimitBits)
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	got, err := calcNextRequiredDifficultyLWMA3(lastNode, lwma3TestWindow, ctx)
	require.NoError(t, err)
	require.Equal(t, lastNode.Bits(), got)
}

// TestLWMA3StableWindowUnchanged confirms a window where every block landed
// exactly on target leaves the target unchanged, i.e. the k = N(N+1)/2*T
// normalization is correct.
func TestLWMA3StableWindowUnchanged(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	// A magnitude chosen so it round-trips through the compact encoding
	// exactly (mantissa 1, well clear of the 23-bit mantissa limit even
	// after the 2x/0.5x scaling used by the sibling tests below).
	baseTarget := new(big.Int).Lsh(big.NewInt(1), 200)
	midBits := BigToCompact(baseTarget)
	require.Zero(t, CompactToBig(midBits).Cmp(baseTarget),
		"test fixture target must round-trip through compact encoding exactly")

	timestamps := lwma3SteadyTimestamps(1_000_000, lwma3TestWindow, targetSecs)
	bits := uniformBits(lwma3TestWindow, midBits)
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	got, err := calcNextRequiredDifficultyLWMA3(lastNode, lwma3TestWindow, ctx)
	require.NoError(t, err)
	require.Equal(t, midBits, got)
}

// TestLWMA3EasesWhenBlocksSlow confirms that a window where every block
// arrived exactly 2x slower than target doubles the next target (halves
// difficulty) — an exact check, not just a directional one.
func TestLWMA3EasesWhenBlocksSlow(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	baseTarget := new(big.Int).Lsh(big.NewInt(1), 200)
	midBits := BigToCompact(baseTarget)
	wantBits := BigToCompact(new(big.Int).Lsh(baseTarget, 1)) // 2x

	timestamps := lwma3SteadyTimestamps(1_000_000, lwma3TestWindow, targetSecs*2)
	bits := uniformBits(lwma3TestWindow, midBits)
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	got, err := calcNextRequiredDifficultyLWMA3(lastNode, lwma3TestWindow, ctx)
	require.NoError(t, err)
	require.Equal(t, wantBits, got)
}

// TestLWMA3TightensWhenBlocksFast is the mirror of the slowdown case: every
// block arriving 2x faster than target halves the next target (doubles
// difficulty).
func TestLWMA3TightensWhenBlocksFast(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	baseTarget := new(big.Int).Lsh(big.NewInt(1), 200)
	midBits := BigToCompact(baseTarget)
	wantBits := BigToCompact(new(big.Int).Rsh(baseTarget, 1)) // 0.5x

	timestamps := lwma3SteadyTimestamps(1_000_000, lwma3TestWindow, targetSecs/2)
	bits := uniformBits(lwma3TestWindow, midBits)
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	got, err := calcNextRequiredDifficultyLWMA3(lastNode, lwma3TestWindow, ctx)
	require.NoError(t, err)
	require.Equal(t, wantBits, got)
}

// TestLWMA3NeverExceedsPowLimit confirms that even a maximally-clamped slow
// window starting already at PowLimit doesn't overflow past it.
func TestLWMA3NeverExceedsPowLimit(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	// 100x target solvetime -- clamps to the 6x ceiling internally, and
	// starting from PowLimitBits leaves no room to grow further.
	timestamps := lwma3SteadyTimestamps(1_000_000, lwma3TestWindow, targetSecs*100)
	bits := uniformBits(lwma3TestWindow, params.PowLimitBits)
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	got, err := calcNextRequiredDifficultyLWMA3(lastNode, lwma3TestWindow, ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, CompactToBig(got).Cmp(params.PowLimit), 0)
}

// TestLWMA3JumpAttackClamp is the adversarial test that justifies choosing
// LWMA-3 over LWMA-1: LWMA-1 has a documented "jump attack" where a miner
// sets one block's timestamp to an extreme value to drag the weighted
// average (and therefore the next target) far more than any real hashrate
// change would justify -- worse the more recent (higher-weighted) the
// manipulated block is. LWMA-3's fix is clamping each individual solvetime
// to +/-6*targetTimePerBlock before it's weighted. This test proves that
// clamp actually bounds the miner's leverage, for both directions of the
// attack, on the single highest-weight (most recent) block in the window --
// the position where the attack does the most damage.
func TestLWMA3JumpAttackClamp(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	baseTarget := new(big.Int).Lsh(big.NewInt(1), 200)
	midBits := BigToCompact(baseTarget)
	bits := uniformBits(lwma3TestWindow, midBits)

	// steadyThenJump builds a window where every block up through the
	// second-to-last landed exactly on target, and only the final
	// (most-recent, highest-weighted) block's solvetime is the attacker's
	// chosen value.
	steadyThenJump := func(jumpSolvetime int64) *blockNode {
		timestamps := lwma3SteadyTimestamps(1_000_000, lwma3TestWindow-1, targetSecs)
		timestamps = append(timestamps, timestamps[len(timestamps)-1]+jumpSolvetime)
		return buildLWMA3Chain(t, timestamps, bits)
	}

	t.Run("future_jump_clamped_to_ceiling", func(t *testing.T) {
		atCeiling := steadyThenJump(6 * targetSecs)
		wayBeyond := steadyThenJump(1_000_000 * targetSecs)

		bitsAtCeiling, err := calcNextRequiredDifficultyLWMA3(atCeiling, lwma3TestWindow, ctx)
		require.NoError(t, err)
		bitsWayBeyond, err := calcNextRequiredDifficultyLWMA3(wayBeyond, lwma3TestWindow, ctx)
		require.NoError(t, err)

		require.Equal(t, bitsAtCeiling, bitsWayBeyond,
			"a solvetime far beyond the 6x ceiling must produce the identical "+
				"result as landing exactly on the ceiling")

		// The clamped result must be far below what an unclamped LWMA-1-style
		// calculation would have produced for the same manipulated timestamp,
		// proving the clamp is actually doing something and isn't a no-op.
		const n = int64(lwma3TestWindow)
		k := big.NewInt(n * (n + 1) / 2 * targetSecs)
		honestWeightedSum := targetSecs * (n - 1) * n / 2 // j=1..N-1 at target
		unclampedWeightedSum := honestWeightedSum + 1_000_000*targetSecs*n
		unclampedTarget := new(big.Int).Mul(baseTarget, big.NewInt(unclampedWeightedSum))
		unclampedTarget.Div(unclampedTarget, k)

		require.Equal(t, -1, CompactToBig(bitsWayBeyond).Cmp(unclampedTarget),
			"clamped target must be far smaller than the unclamped-formula "+
				"target an LWMA-1-style calculation would have produced")
	})

	t.Run("past_jump_clamped_to_floor", func(t *testing.T) {
		atFloor := steadyThenJump(-6 * targetSecs)
		wayBeyond := steadyThenJump(-1_000_000 * targetSecs)

		bitsAtFloor, err := calcNextRequiredDifficultyLWMA3(atFloor, lwma3TestWindow, ctx)
		require.NoError(t, err)
		bitsWayBeyond, err := calcNextRequiredDifficultyLWMA3(wayBeyond, lwma3TestWindow, ctx)
		require.NoError(t, err)

		require.Equal(t, bitsAtFloor, bitsWayBeyond,
			"a solvetime far beyond the -6x floor must produce the identical "+
				"result as landing exactly on the floor")

		// A backdated timestamp attack tries to spike difficulty (shrink the
		// target) far past what a real speedup would justify. Confirm the
		// clamped result stays well above what an unclamped calculation
		// would have collapsed the target to (or would have gone negative,
		// which isn't even a valid target).
		const n = int64(lwma3TestWindow)
		honestWeightedSum := targetSecs * (n - 1) * n / 2
		unclampedWeightedSum := honestWeightedSum - 1_000_000*targetSecs*n
		require.Negative(t, unclampedWeightedSum,
			"fixture sanity check: this attack magnitude should drive the "+
				"unclamped weighted sum negative, which is the pathological "+
				"case the floor in calcNextRequiredDifficultyLWMA3 guards against")

		require.Equal(t, 1, CompactToBig(bitsWayBeyond).Cmp(big.NewInt(0)),
			"clamped target must still be a valid positive target")
	})
}

// TestLWMA3AllSolvetimesNegativeFloored exercises the defensive floor for
// the pathological case where every block in the window is backdated to the
// -6x clamp, which would otherwise drive the weighted solvetime sum
// negative and produce an invalid (non-positive) target.
func TestLWMA3AllSolvetimesNegativeFloored(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	baseTarget := new(big.Int).Lsh(big.NewInt(1), 200)
	midBits := BigToCompact(baseTarget)
	bits := uniformBits(lwma3TestWindow, midBits)

	// Every block dated exactly at the parent's timestamp minus a huge
	// offset, so every solvetime clamps to -6x.
	timestamps := make([]int64, lwma3TestWindow+1)
	timestamps[0] = 10_000_000
	for i := int32(1); i <= lwma3TestWindow; i++ {
		timestamps[i] = timestamps[i-1] - 1_000*targetSecs
	}
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	got, err := calcNextRequiredDifficultyLWMA3(lastNode, lwma3TestWindow, ctx)
	require.NoError(t, err)
	require.Equal(t, 1, CompactToBig(got).Cmp(big.NewInt(0)),
		"target must remain positive even when every solvetime floors the weighted sum")
	require.Equal(t, -1, CompactToBig(got).Cmp(baseTarget),
		"an all-backdated window should sharply increase difficulty (shrink the target)")
}

// TestLWMA3PoWNoRetargeting confirms regtest-style networks that disable
// retargeting entirely get PowLimitBits unconditionally, matching the
// existing calcNextRequiredDifficulty behavior for the same flag.
func TestLWMA3PoWNoRetargeting(t *testing.T) {
	params := chaincfg.RegressionNetParams
	require.True(t, params.PoWNoRetargeting)
	ctx := stubChainCtx{params: &params}

	targetSecs := int64(params.TargetTimePerBlock / time.Second)
	timestamps := lwma3SteadyTimestamps(1_000_000, lwma3TestWindow, targetSecs)
	bits := uniformBits(lwma3TestWindow, params.PowLimitBits)
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	got, err := calcNextRequiredDifficultyLWMA3(lastNode, lwma3TestWindow, ctx)
	require.NoError(t, err)
	require.Equal(t, params.PowLimitBits, got)
}
