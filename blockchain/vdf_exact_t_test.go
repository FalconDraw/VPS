// Copyright (c) 2013-2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// vdfTChain links a chain of blockNodes windowBlocks+1 long (oldest first),
// each spacing seconds after the last, ending at height windowBlocks -- the
// minimum needed for a full VDFTWindowBlocks-sized expectedVDFT window.
// Mirrors buildLWMA3Chain's shape for the same reason: expectedVDFT walks
// ancestors the same way calcNextRequiredDifficultyLWMA3 does.
func vdfTChain(t *testing.T, windowBlocks int32, spacing int64) *blockNode {
	t.Helper()
	var parent *blockNode
	var last *blockNode
	ts := int64(1_000_000)
	for i := int32(0); i <= windowBlocks; i++ {
		header := &wire.BlockHeader{Timestamp: time.Unix(ts, 0)}
		node := newBlockNode(header, parent)
		last = node
		parent = node
		ts += spacing
	}
	return last
}

// expectedT is the same formula expectedVDFT implements, computed
// independently here so tests aren't just re-asserting the implementation
// against itself.
func expectedT(spanSecs, spanBlocks int64) uint64 {
	T := uint64(chaincfg.VDFTargetBlocks * spanSecs * 1000 /
		(chaincfg.VDFReferenceMsPerStep * spanBlocks))
	if T < chaincfg.VDFMinT {
		T = chaincfg.VDFMinT
	}
	return T
}

// TestExpectedVDFTStableWindow confirms a full window of blocks landing
// exactly on TargetTimePerBlock produces the un-clamped formula result.
func TestExpectedVDFTStableWindow(t *testing.T) {
	params := chaincfg.MainNetParams
	b := &BlockChain{chainParams: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	drawNode := vdfTChain(t, chaincfg.VDFTWindowBlocks, targetSecs)

	spanBlocks := int64(chaincfg.VDFTWindowBlocks)
	spanSecs := spanBlocks * targetSecs
	want := expectedT(spanSecs, spanBlocks)

	got := b.expectedVDFT(drawNode)
	require.Equal(t, want, got)
	// Sanity: the un-clamped path was actually exercised, not a clamp.
	minSpan := spanBlocks * targetSecs / 10
	maxSpan := spanBlocks * targetSecs * 4
	require.Greater(t, spanSecs, minSpan)
	require.Less(t, spanSecs, maxSpan)
}

// TestExpectedVDFTClampsLowSpan confirms blocks landing far faster than
// target (a burst) clamp the window's average spacing at
// TargetTimePerBlock/10 per block rather than letting T shrink
// proportionally to the raw (much faster) pace -- this is the leverage
// bound that stops a miner from shrinking T by compressing block times.
func TestExpectedVDFTClampsLowSpan(t *testing.T) {
	params := chaincfg.MainNetParams
	b := &BlockChain{chainParams: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	// 1 second per block: far below the clamp floor.
	drawNode := vdfTChain(t, chaincfg.VDFTWindowBlocks, 1)

	spanBlocks := int64(chaincfg.VDFTWindowBlocks)
	minSpan := spanBlocks * targetSecs / 10
	want := expectedT(minSpan, spanBlocks)

	got := b.expectedVDFT(drawNode)
	require.Equal(t, want, got)

	// Confirm the clamp actually changed the result relative to the raw
	// (unclamped) pace -- otherwise this test wouldn't be exercising the
	// clamp branch at all.
	rawSpan := spanBlocks * 1
	require.NotEqual(t, expectedT(rawSpan, spanBlocks), got)
}

// TestExpectedVDFTClampsHighSpan is the mirror case: blocks landing far
// slower than target (a stall) clamp the average spacing at
// 4*TargetTimePerBlock per block rather than letting T grow unbounded.
func TestExpectedVDFTClampsHighSpan(t *testing.T) {
	params := chaincfg.MainNetParams
	b := &BlockChain{chainParams: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	// 100x target spacing: far above the clamp ceiling.
	drawNode := vdfTChain(t, chaincfg.VDFTWindowBlocks, targetSecs*100)

	spanBlocks := int64(chaincfg.VDFTWindowBlocks)
	maxSpan := spanBlocks * targetSecs * 4
	want := expectedT(maxSpan, spanBlocks)

	got := b.expectedVDFT(drawNode)
	require.Equal(t, want, got)

	rawSpan := spanBlocks * targetSecs * 100
	require.NotEqual(t, expectedT(rawSpan, spanBlocks), got)
}

// TestExpectedVDFTShortWindowUsesPartialSpan confirms a draw block shallower
// than VDFTWindowBlocks (e.g. early chain) uses the shorter real span down
// to genesis rather than requiring a full 36-block history -- and that at
// a steady on-target pace, the result matches the full-window case, since
// the formula is rate-based (spanSecs/spanBlocks), not window-size-based.
func TestExpectedVDFTShortWindowUsesPartialSpan(t *testing.T) {
	params := chaincfg.MainNetParams
	b := &BlockChain{chainParams: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	const shortWindow = int32(10) // less than VDFTWindowBlocks (36)
	require.Less(t, shortWindow, chaincfg.VDFTWindowBlocks)

	drawNode := vdfTChain(t, shortWindow, targetSecs)
	got := b.expectedVDFT(drawNode)

	fullWindowNode := vdfTChain(t, chaincfg.VDFTWindowBlocks, targetSecs)
	wantSameAsFullWindow := b.expectedVDFT(fullWindowNode)

	require.Equal(t, wantSameAsFullWindow, got,
		"a steady on-target rate must produce the same T regardless of how much history is available")
}

// TestExpectedVDFTGenesisDrawFallsBackToInitialT confirms a draw block at
// height 0 (spanBlocks == 0, no ancestor window at all) returns
// VDFInitialT rather than dividing by zero.
func TestExpectedVDFTGenesisDrawFallsBackToInitialT(t *testing.T) {
	params := chaincfg.MainNetParams
	b := &BlockChain{chainParams: &params}

	genesisOnly := vdfTChain(t, 0, 0)
	require.Zero(t, genesisOnly.height)

	got := b.expectedVDFT(genesisOnly)
	require.Equal(t, chaincfg.VDFInitialT, got)
}

// TestExpectedVDFTDeterministicAcrossRepeatedCalls confirms expectedVDFT is
// a pure function of chain state -- calling it twice for the same node
// must yield the same result. This is the property validateVDFSettlement's
// hard equality check depends on: the submission path (startVDFComputation)
// and the validation path both call this function independently, and any
// non-determinism here would make a legitimate VDFResult unvalidatable.
func TestExpectedVDFTDeterministicAcrossRepeatedCalls(t *testing.T) {
	params := chaincfg.MainNetParams
	b := &BlockChain{chainParams: &params}
	targetSecs := int64(params.TargetTimePerBlock / time.Second)

	// An irregular (non-uniform) spacing pattern, closer to a real chain
	// than the steady-rate fixtures above.
	var parent *blockNode
	var last *blockNode
	ts := int64(1_000_000)
	spacings := []int64{300, 900, 150, 1200, 600, 45, 2000, 600, 600, 800}
	for len(spacings) <= int(chaincfg.VDFTWindowBlocks) {
		spacings = append(spacings, targetSecs)
	}
	for i := 0; i <= int(chaincfg.VDFTWindowBlocks); i++ {
		header := &wire.BlockHeader{Timestamp: time.Unix(ts, 0)}
		node := newBlockNode(header, parent)
		last = node
		parent = node
		if i < len(spacings) {
			ts += spacings[i]
		}
	}

	first := b.expectedVDFT(last)
	second := b.expectedVDFT(last)
	require.Equal(t, first, second)
}

// TestCalcNextVDFTPostActivationMatchesDirectCall confirms that once
// active, CalcNextVDFT's dispatch to expectedVDFT for a historical draw
// height produces exactly the value directly calling expectedVDFT on that
// same node would -- i.e. the RPC/mining-facing entry point doesn't
// silently diverge from the value validateVDFSettlement will enforce for
// the same draw.
func TestCalcNextVDFTPostActivationMatchesDirectCall(t *testing.T) {
	params := chaincfg.MainNetParams
	targetSecs := int64(params.TargetTimePerBlock / time.Second)
	tip := vdfTChain(t, chaincfg.VDFTWindowBlocks*2, targetSecs)
	params.VDFExactTActivationHeight = 1 // active from height 1

	b := &BlockChain{chainParams: &params, bestChain: newChainView(tip)}

	drawHeight := uint32(chaincfg.VDFTWindowBlocks) // a historical height with a full window behind it
	drawNode := b.bestChain.NodeByHeight(int32(drawHeight))
	require.NotNil(t, drawNode)

	want := b.expectedVDFT(drawNode)
	got := b.CalcNextVDFT(drawHeight)
	require.Equal(t, want, got)
}

// TestCalcNextVDFTBelowActivationUsesLegacyPath confirms CalcNextVDFT does
// not take the expectedVDFT dispatch before VDFExactTActivationHeight,
// mirroring TestLWMA3ActivationSwitch's pattern for the difficulty
// retarget's own flag day. Height 0 is used so the legacy path's own
// early-chain short-circuit (curHeight <= oldHeight) returns VDFInitialT
// without needing a database-backed BlockByHeight lookup.
func TestCalcNextVDFTBelowActivationUsesLegacyPath(t *testing.T) {
	params := chaincfg.MainNetParams
	params.VDFExactTActivationHeight = 0 // disabled
	tip := vdfTChain(t, 0, 0)            // genesis-only chain, height 0
	b := &BlockChain{chainParams: &params, bestChain: newChainView(tip),
		stateSnapshot: &BestState{Height: 0}}

	got := b.CalcNextVDFT(0)
	require.Equal(t, chaincfg.VDFInitialT, got)
}
