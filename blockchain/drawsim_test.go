package blockchain

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
)

// cryptoSeed returns a 64-bit seed from crypto/rand.
func cryptoSeed() int64 {
	var b [8]byte
	rand.Read(b[:]) //nolint:errcheck
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// stakeScenario holds pre-built StakeEntry slices for a scenario.
// Block hashes are randomised per draw; stake positions are fixed.
type stakeScenario struct {
	stakes []StakeEntry
}

// buildScenario pre-generates numStakers wallets, each with tokensEach tokens.
func buildScenario(rng *mrand.Rand, numStakers int, tokensEach int64) stakeScenario {
	stakes := make([]StakeEntry, numStakers)
	for i := 0; i < numStakers; i++ {
		var wh [20]byte
		rng.Read(wh[:]) //nolint:errcheck
		wh[0] |= 0x01   // avoid the zero sentinel
		atoms := tokensEach * 1e8
		stakes[i] = StakeEntry{WalletHash: wh, TokenCount: tokensEach, TotalValue: atoms}
	}
	return stakeScenario{stakes: stakes}
}

// buildUnequalScenario builds a 3-staker scenario with 1, 10, and 100 tokens.
func buildUnequalScenario(rng *mrand.Rand) stakeScenario {
	tokenCounts := []int64{1, 10, 100}
	stakes := make([]StakeEntry, 3)
	for i, tc := range tokenCounts {
		var wh [20]byte
		rng.Read(wh[:]) //nolint:errcheck
		wh[0] |= 0x01
		atoms := tc * 1e8
		stakes[i] = StakeEntry{WalletHash: wh, TokenCount: tc, TotalValue: atoms}
	}
	return stakeScenario{stakes: stakes}
}

// TestWinFrequencySimulation runs the actual RunDraw against random block hashes
// and verifies the jackpot frequency is statistically close to 1/WinMultiplier.
//
// Run with: go test -v -run TestWinFrequencySimulation -count=1
func TestWinFrequencySimulation(t *testing.T) {
	rng := mrand.New(mrand.NewSource(cryptoSeed()))

	const (
		drawsPerScenario = 500_000
		drawHeight       = uint32(DrawInterval)
	)

	type scenarioDef struct {
		name    string
		numS    int
		tokensE int64
		unequal bool
	}
	defs := []scenarioDef{
		{"1 staker, 1 token", 1, 1, false},
		{"1 staker, 1000 tokens", 1, 1000, false},
		{"5 stakers, 1 token each", 5, 1, false},
		{"5 stakers, 200 tokens each", 5, 200, false},
		{"50 stakers, 10 tokens each", 50, 10, false},
		{"100 stakers, 1 token each", 100, 1, false},
		{"3 stakers: 1, 10, 100 tokens", 0, 0, true},
	}

	want := 1.0 / float64(WinMultiplier) // 10.000%
	// At 500k draws, σ = sqrt(p(1-p)/n) ≈ 0.00042 (0.042%).
	// 5σ tolerance = 0.21% — fail only on genuine bugs, not random noise.
	tolerance := 0.005 // ±0.5 percentage points

	fmt.Printf("\n%-36s  %8s  %8s  %9s  %s\n",
		"Scenario", "Draws", "Wins", "Win%", "Result")
	fmt.Printf("%s\n", "──────────────────────────────────────────────────────────────────────")

	allPassed := true
	for _, def := range defs {
		var sc stakeScenario
		if def.unequal {
			sc = buildUnequalScenario(rng)
		} else {
			sc = buildScenario(rng, def.numS, def.tokensE)
		}

		wins := 0
		var blockHash chainhash.Hash
		for i := 0; i < drawsPerScenario; i++ {
			rng.Read(blockHash[:]) //nolint:errcheck
			result := RunDraw(sc.stakes, CalcTargetHash(blockHash, drawHeight))
			if result.JackpotWon {
				wins++
			}
		}

		pct := float64(wins) / float64(drawsPerScenario) * 100
		delta := pct/100 - want
		if delta < 0 {
			delta = -delta
		}
		status := "PASS"
		if delta > tolerance {
			status = "FAIL"
			allPassed = false
		}
		fmt.Printf("%-36s  %8d  %8d  %8.4f%%  %s\n",
			def.name, drawsPerScenario, wins, pct, status)
	}

	fmt.Printf("\nExpected: %.1f%%  (WinMultiplier=%d)  Tolerance: ±%.1f%%\n\n",
		want*100, WinMultiplier, tolerance*100)

	if !allPassed {
		t.Error("one or more scenarios deviated beyond tolerance — mechanic may be broken")
	}
}

// TestStreakProbability reports the probability of N consecutive non-winning draws.
// 15 draws with no win has P = 0.9^15 ≈ 20.6% — not statistically unusual.
func TestStreakProbability(t *testing.T) {
	fmt.Printf("\nProbability of N consecutive no-win draws (10%% chance each):\n\n")
	fmt.Printf("%-6s  %14s\n", "Draws", "P(all miss)")
	fmt.Printf("───────────────────────\n")
	p := 1.0
	for n := 1; n <= 30; n++ {
		p *= (1.0 - 1.0/float64(WinMultiplier))
		marker := ""
		if n == 15 {
			marker = "  ← you are here"
		}
		fmt.Printf("%-6d  %13.4f%%%s\n", n, p*100, marker)
	}
	fmt.Println()
}
