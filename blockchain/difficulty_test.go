package blockchain

import (
	"math/big"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// emergencyTestNode builds a standalone blockNode at a non-retarget height
// with the given timestamp/bits, sufficient for exercising the "not a
// retarget interval" branch of calcNextRequiredDifficulty without needing a
// full linked chain.
func emergencyTestNode(height int32, timestamp int64, bits uint32) *blockNode {
	header := &wire.BlockHeader{
		Bits:      bits,
		Timestamp: time.Unix(timestamp, 0),
	}
	node := newBlockNode(header, nil)
	node.height = height
	return node
}

// TestEmergencyAdjustWindowDisabled confirms networks with
// EmergencyAdjustWindow unset (e.g. regtest) get the plain
// "return previous difficulty unchanged" behavior, not an emergency ease-down.
func TestEmergencyAdjustWindowDisabled(t *testing.T) {
	params := chaincfg.RegressionNetParams
	require.Zero(t, params.EmergencyAdjustWindow)

	ctx := stubChainCtx{params: &params}
	lastNode := emergencyTestNode(1, 1_000_000, params.PowLimitBits)

	// Ten hours late — would trigger the emergency rule if it were enabled.
	newBlockTime := time.Unix(1_000_000+10*60*60, 0)
	bits, err := calcNextRequiredDifficulty(lastNode, newBlockTime, ctx)
	require.NoError(t, err)
	require.Equal(t, lastNode.Bits(), bits, "difficulty must be unchanged when EmergencyAdjustWindow is disabled")
}

// TestEmergencyAdjustWindowNotYetElapsed confirms a block arriving within the
// window gets the parent's unchanged difficulty — no premature easing.
func TestEmergencyAdjustWindowNotYetElapsed(t *testing.T) {
	params := chaincfg.MainNetParams
	require.Equal(t, 10*time.Minute, params.EmergencyAdjustWindow)
	// Isolate the window-compounding logic under test from
	// EmergencyAdjustActivationHeight (a separate concern covered by
	// TestEmergencyAdjustActivationHeightGate) by keeping the ease-down
	// active from genesis here.
	params.EmergencyAdjustActivationHeight = 0

	ctx := stubChainCtx{params: &params}
	// Use a mid-range difficulty (not PowLimitBits) so an accidental
	// "clamp to PowLimit" bug wouldn't be masked by a no-op.
	midBits := uint32(0x1c00ffff)
	lastNode := emergencyTestNode(100, 1_000_000, midBits)

	// Nine minutes later — under the 10 minute window.
	newBlockTime := time.Unix(1_000_000+9*60, 0)
	bits, err := calcNextRequiredDifficulty(lastNode, newBlockTime, ctx)
	require.NoError(t, err)
	require.Equal(t, midBits, bits)
}

// TestEmergencyAdjustWindowCompounds confirms each additional full window
// elapsed eases difficulty by another factor of RetargetAdjustmentFactor,
// i.e. the target grows (difficulty drops) geometrically.
func TestEmergencyAdjustWindowCompounds(t *testing.T) {
	params := chaincfg.MainNetParams
	// Isolate the compounding math from EmergencyAdjustActivationHeight (a
	// separate concern covered by TestEmergencyAdjustActivationHeightGate).
	params.EmergencyAdjustActivationHeight = 0
	ctx := stubChainCtx{params: &params}
	midBits := uint32(0x1b0404cb) // an arbitrary non-trivial mid-range target
	lastNode := emergencyTestNode(100, 1_000_000, midBits)

	baseTarget := CompactToBig(midBits)
	factor := params.RetargetAdjustmentFactor

	testCases := []struct {
		name    string
		periods int64 // full 10-minute windows elapsed
	}{
		{"one_period", 1},
		{"two_periods", 2},
		{"three_periods", 3},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			elapsedSecs := int64(params.EmergencyAdjustWindow/time.Second)*tc.periods + 1
			newBlockTime := time.Unix(1_000_000+elapsedSecs, 0)

			bits, err := calcNextRequiredDifficulty(lastNode, newBlockTime, ctx)
			require.NoError(t, err)

			wantTarget := new(big.Int).Set(baseTarget)
			for i := int64(0); i < tc.periods; i++ {
				wantTarget.Mul(wantTarget, big.NewInt(factor))
			}
			wantBits := BigToCompact(wantTarget)

			require.Equal(t, wantBits, bits,
				"periods=%d: got target %s, want %s",
				tc.periods, CompactToBig(bits), wantTarget)

			// Difficulty must have strictly eased (target grew) relative to parent.
			require.Equal(t, 1, CompactToBig(bits).Cmp(baseTarget),
				"target should have grown (difficulty eased) after %d stalled window(s)", tc.periods)
		})
	}
}

// TestEmergencyAdjustWindowCapped confirms a single block can't claim an
// unbounded difficulty crash via an inflated timestamp gap — the number of
// windows applied per block is capped, and further easing only accrues
// across subsequent blocks, not from one oversized timestamp jump.
func TestEmergencyAdjustWindowCapped(t *testing.T) {
	params := chaincfg.MainNetParams
	// Isolate the per-block cap from EmergencyAdjustActivationHeight (a
	// separate concern covered by TestEmergencyAdjustActivationHeightGate).
	params.EmergencyAdjustActivationHeight = 0
	ctx := stubChainCtx{params: &params}
	midBits := uint32(0x1b0404cb)
	lastNode := emergencyTestNode(100, 1_000_000, midBits)

	windowSecs := int64(params.EmergencyAdjustWindow / time.Second)

	// Just at the cap (6 windows) vs. wildly beyond it (60 windows) must
	// produce the identical, capped result.
	atCapTime := time.Unix(1_000_000+windowSecs*6+1, 0)
	wayBeyondCapTime := time.Unix(1_000_000+windowSecs*60, 0)

	bitsAtCap, err := calcNextRequiredDifficulty(lastNode, atCapTime, ctx)
	require.NoError(t, err)
	bitsBeyondCap, err := calcNextRequiredDifficulty(lastNode, wayBeyondCapTime, ctx)
	require.NoError(t, err)

	require.Equal(t, bitsAtCap, bitsBeyondCap,
		"a single block's emergency easing must be capped regardless of how far the timestamp overshoots the cap")
}

// TestEmergencyAdjustWindowNeverExceedsPowLimit confirms compounding easing
// clamps at the network's proof-of-work floor rather than overflowing past it.
func TestEmergencyAdjustWindowNeverExceedsPowLimit(t *testing.T) {
	params := chaincfg.MainNetParams
	// Isolate the PowLimit clamp from EmergencyAdjustActivationHeight (a
	// separate concern covered by TestEmergencyAdjustActivationHeightGate).
	params.EmergencyAdjustActivationHeight = 0
	ctx := stubChainCtx{params: &params}
	// Start already very close to PowLimit so even one or two easing steps
	// would blow past it without clamping.
	lastNode := emergencyTestNode(100, 1_000_000, params.PowLimitBits)

	windowSecs := int64(params.EmergencyAdjustWindow / time.Second)
	newBlockTime := time.Unix(1_000_000+windowSecs*6+1, 0)

	bits, err := calcNextRequiredDifficulty(lastNode, newBlockTime, ctx)
	require.NoError(t, err)
	require.LessOrEqual(t, CompactToBig(bits).Cmp(params.PowLimit), 0,
		"eased target must never exceed the network's PowLimit")
}

// TestLWMA3ActivationSwitch confirms calcNextRequiredDifficulty dispatches to
// the LWMA-3 path at and after LWMA3ActivationHeight, and still uses the
// legacy/EmergencyAdjustWindow path the block before. It distinguishes the
// two paths with a scenario where they'd disagree: a large post-parent gap
// that the emergency ease-down would react to, on a chain shorter than
// LWMA3WindowSize so the LWMA-3 "not enough history" fallback leaves
// difficulty unchanged instead.
func TestLWMA3ActivationSwitch(t *testing.T) {
	params := chaincfg.MainNetParams
	// Isolate the LWMA-3 dispatch switch from EmergencyAdjustActivationHeight
	// (a separate concern covered by TestEmergencyAdjustActivationHeightGate)
	// by keeping the emergency ease-down active from genesis here.
	params.EmergencyAdjustActivationHeight = 0
	params.LWMA3ActivationHeight = 41
	ctx := stubChainCtx{params: &params}

	midBits := uint32(0x1b0404cb)
	windowSecs := int64(params.EmergencyAdjustWindow / time.Second)
	// Big enough gap to trigger a multi-window emergency ease-down if the
	// legacy path ran.
	newBlockTime := time.Unix(1_000_000+windowSecs*3+1, 0)

	// height+1 == 41 == activation: LWMA-3 dispatches. lastNode.Height()
	// (40) is below LWMA3WindowSize (60), so LWMA-3's own "not enough
	// history" fallback returns the parent's bits unchanged — proving the
	// emergency ease-down (which would have changed them) did not run.
	atActivation := emergencyTestNode(40, 1_000_000, midBits)
	bits, err := calcNextRequiredDifficulty(atActivation, newBlockTime, ctx)
	require.NoError(t, err)
	require.Equal(t, midBits, bits,
		"at activation height, LWMA-3 dispatch must run instead of the emergency ease-down")

	// height+1 == 40 < activation (41): still the legacy path, so the same
	// gap does ease difficulty.
	beforeActivation := emergencyTestNode(39, 1_000_000, midBits)
	preBits, err := calcNextRequiredDifficulty(beforeActivation, newBlockTime, ctx)
	require.NoError(t, err)
	require.NotEqual(t, midBits, preBits,
		"the block immediately before activation must still run the emergency ease-down")
}

// TestLWMA3ActivationDispatchMatchesDirectCall confirms that once active,
// calcNextRequiredDifficulty's dispatch to LWMA-3 produces exactly the same
// result as calling calcNextRequiredDifficultyLWMA3 directly with the
// production LWMA3WindowSize — i.e. the wiring doesn't alter the algorithm's
// output for a real full-size window.
func TestLWMA3ActivationDispatchMatchesDirectCall(t *testing.T) {
	params := chaincfg.MainNetParams
	params.LWMA3ActivationHeight = 1 // active from height 1, i.e. for this whole test chain
	ctx := stubChainCtx{params: &params}

	targetSecs := int64(params.TargetTimePerBlock / time.Second)
	timestamps := lwma3SteadyTimestamps(1_000_000, LWMA3WindowSize, targetSecs)
	bits := uniformBits(LWMA3WindowSize, params.PowLimitBits)
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	// newBlockTime is irrelevant to LWMA-3 (it only reads ancestor
	// timestamps), but calcNextRequiredDifficulty's signature requires one.
	newBlockTime := time.Unix(timestamps[len(timestamps)-1]+targetSecs, 0)

	got, err := calcNextRequiredDifficulty(lastNode, newBlockTime, ctx)
	require.NoError(t, err)
	want, err := calcNextRequiredDifficultyLWMA3(lastNode, LWMA3WindowSize, ctx)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestEmergencyAdjustActivationHeightGate confirms the block immediately
// below EmergencyAdjustActivationHeight validates under the pre-emergency
// carry-over rule (difficulty unchanged regardless of gap) while the block
// at the activation height applies the ease-down — the fix for a fresh node
// re-validating history rejecting pre-deployment blocks that spanned a quiet
// gap, since the ease-down would otherwise retroactively change the
// expected difficulty for blocks mined before the rule existed.
func TestEmergencyAdjustActivationHeightGate(t *testing.T) {
	params := chaincfg.MainNetParams
	require.NotZero(t, params.EmergencyAdjustActivationHeight,
		"fixture sanity check: this test is meaningless if mainnet's gate is unset")
	activation := params.EmergencyAdjustActivationHeight

	ctx := stubChainCtx{params: &params}
	midBits := uint32(0x1b0404cb)

	// Ten hours late — well past a single window, would compound multiple
	// easing steps if the emergency rule applied.
	windowSecs := int64(params.EmergencyAdjustWindow / time.Second)
	newBlockTime := time.Unix(1_000_000+windowSecs*6, 0)

	// height+1 == activation-1: below the gate, pre-emergency carry-over
	// rule applies regardless of how late the block is.
	belowGate := emergencyTestNode(activation-2, 1_000_000, midBits)
	belowBits, err := calcNextRequiredDifficulty(belowGate, newBlockTime, ctx)
	require.NoError(t, err)
	require.Equal(t, midBits, belowBits,
		"below EmergencyAdjustActivationHeight, difficulty must carry over unchanged even after a long gap")

	// height+1 == activation: gate opens, the same gap now eases difficulty.
	atGate := emergencyTestNode(activation-1, 1_000_000, midBits)
	atBits, err := calcNextRequiredDifficulty(atGate, newBlockTime, ctx)
	require.NoError(t, err)
	require.NotEqual(t, midBits, atBits,
		"at EmergencyAdjustActivationHeight, the ease-down must apply")
}

// TestLWMA3StallEaseBelowThreshold confirms a gap at or under
// LWMA3SolveTimeClampMultiplier*TargetTimePerBlock leaves the LWMA-3 base
// target untouched — this threshold intentionally matches LWMA-3's own
// per-solvetime clamp, so ordinary variance that LWMA-3 already considers
// normal must not additionally trigger the stall ease.
func TestLWMA3StallEaseBelowThreshold(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)
	threshold := LWMA3SolveTimeClampMultiplier * targetSecs
	baseBits := uint32(0x1b0404cb)

	got := applyLWMA3StallEase(baseBits, 1_000_000, 1_000_000+threshold, ctx)
	require.Equal(t, baseBits, got,
		"a gap exactly at the threshold must not ease the base target")
}

// TestLWMA3StallEaseCompounds confirms each additional full
// LWMA3SolveTimeClampMultiplier*TargetTimePerBlock window past the
// threshold eases the LWMA-3 base target by another factor of
// RetargetAdjustmentFactor, compounding — the same shape as the legacy
// EmergencyAdjustWindow mechanism (TestEmergencyAdjustWindowCompounds), just
// keyed to LWMA-3's wider threshold and layered on a non-trivial base.
func TestLWMA3StallEaseCompounds(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)
	windowSecs := LWMA3SolveTimeClampMultiplier * targetSecs
	baseBits := uint32(0x1b0404cb)
	baseTarget := CompactToBig(baseBits)
	factor := params.RetargetAdjustmentFactor

	for _, periods := range []int64{1, 2, 3} {
		elapsed := windowSecs*periods + 1
		got := applyLWMA3StallEase(baseBits, 1_000_000, 1_000_000+elapsed, ctx)

		wantTarget := new(big.Int).Set(baseTarget)
		for i := int64(0); i < periods; i++ {
			wantTarget.Mul(wantTarget, big.NewInt(factor))
		}
		require.Equal(t, BigToCompact(wantTarget), got,
			"periods=%d: got target %s, want %s", periods, CompactToBig(got), wantTarget)
	}
}

// TestLWMA3StallEaseNeverExceedsPowLimit mirrors
// TestEmergencyAdjustWindowNeverExceedsPowLimit for the new stall-ease path:
// an extreme gap must clamp at PowLimit rather than overflow past it.
func TestLWMA3StallEaseNeverExceedsPowLimit(t *testing.T) {
	params := chaincfg.MainNetParams
	ctx := stubChainCtx{params: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)
	windowSecs := LWMA3SolveTimeClampMultiplier * targetSecs

	// Start already very close to PowLimit so even one easing step would
	// blow past it without clamping.
	nearLimit := new(big.Int).Rsh(params.PowLimit, 1) // PowLimit / 2
	baseBits := BigToCompact(nearLimit)

	got := applyLWMA3StallEase(baseBits, 1_000_000, 1_000_000+windowSecs*10+1, ctx)
	require.LessOrEqual(t, CompactToBig(got).Cmp(params.PowLimit), 0,
		"eased target must never exceed the network's PowLimit")
}

// TestLWMA3StallEaseUnsticksATotalStall is the integration-level proof this
// mechanism exists for: with a full LWMA3WindowSize chain of on-time blocks
// (so calcNextRequiredDifficultyLWMA3 itself would otherwise leave
// difficulty unchanged) followed by a real multi-hour gap in which zero
// further blocks landed, calcNextRequiredDifficulty must still ease the
// *next* candidate's required difficulty relative to the pure-LWMA-3 value
// — proving the base can no longer freeze the chain indefinitely just
// because no block has landed to feed LWMA-3's own window.
func TestLWMA3StallEaseUnsticksATotalStall(t *testing.T) {
	params := chaincfg.MainNetParams
	params.LWMA3ActivationHeight = 1
	ctx := stubChainCtx{params: &params}

	targetSecs := int64(params.TargetTimePerBlock / time.Second)
	timestamps := lwma3SteadyTimestamps(1_000_000, LWMA3WindowSize, targetSecs)
	bits := uniformBits(LWMA3WindowSize, params.PowLimitBits)
	lastNode := buildLWMA3Chain(t, timestamps, bits)

	lwma3Only, err := calcNextRequiredDifficultyLWMA3(lastNode, LWMA3WindowSize, ctx)
	require.NoError(t, err)

	// 19 hours since the parent with nothing landing in between — the
	// real-world scenario this mechanism is for.
	stalledTime := time.Unix(timestamps[len(timestamps)-1]+19*60*60, 0)
	got, err := calcNextRequiredDifficulty(lastNode, stalledTime, ctx)
	require.NoError(t, err)

	require.Equal(t, 1, CompactToBig(got).Cmp(CompactToBig(lwma3Only)),
		"a real multi-hour stall must ease the candidate's difficulty below the frozen pure-LWMA-3 value")
}
