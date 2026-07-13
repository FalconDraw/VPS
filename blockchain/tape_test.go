package blockchain

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
)

func TestCalcTargetHash(t *testing.T) {
	var prev chainhash.Hash
	prev[0] = 0x01

	const dh = uint32(DrawInterval)

	// Different draw heights must produce different hashes for the same block.
	h1 := CalcTargetHash(prev, dh)
	h2 := CalcTargetHash(prev, dh+uint32(DrawInterval))
	if h1 == h2 {
		t.Fatal("different draw heights must produce different target hashes")
	}

	// Different block hashes must produce different target hashes.
	var prev2 chainhash.Hash
	prev2[0] = 0x02
	if CalcTargetHash(prev2, dh) == h1 {
		t.Fatal("different blockNHashes must produce different target hashes")
	}
}

func TestSelectWinnerPos(t *testing.T) {
	// All-zeros entropy → pos = 0 for any totalTokens.
	var zero [32]byte
	if pos := SelectWinnerPos(zero, 100); pos != 0 {
		t.Fatalf("zero entropy: want pos=0, got %d", pos)
	}

	// entropy[31] = 15, totalTokens = 7 → 15 mod 7 = 1.
	var e [32]byte
	e[31] = 15
	if pos := SelectWinnerPos(e, 7); pos != 1 {
		t.Fatalf("15 mod 7: want 1, got %d", pos)
	}

	// pos must always be in [0, totalTokens).
	for n := int64(1); n <= 50; n++ {
		for b := byte(0); b < 200; b++ {
			var en [32]byte
			en[0] = b
			en[31] = b ^ 0x5a
			pos := SelectWinnerPos(en, n)
			if pos < 0 || pos >= n {
				t.Fatalf("pos %d out of range [0, %d) for entropy[0]=%d", pos, n, b)
			}
		}
	}
}

func TestRunDraw(t *testing.T) {
	const drawHeight = uint32(DrawInterval)
	const satoshi = int64(1e8)

	var blockNHash chainhash.Hash
	blockNHash[0] = 0xde
	blockNHash[1] = 0xad

	t.Run("empty", func(t *testing.T) {
		r := RunDraw(nil, CalcTargetHash(blockNHash, drawHeight))
		if r.JackpotWon || len(r.Winners) != 0 || r.Rollover != 0 {
			t.Fatal("expected zero result for nil stakes")
		}
	})

	t.Run("value_conservation", func(t *testing.T) {
		// Run many draws; prize+rollover must always equal pool.
		const n = 5
		totalPool := int64(n * satoshi)
		stakes := make([]StakeEntry, n)
		for i := range stakes {
			stakes[i] = StakeEntry{WalletHash: [20]byte{byte(i + 1)}, TokenCount: 1, TotalValue: satoshi}
		}
		var foundJackpot, foundRollover bool
		for attempt := 0; attempt < 200000 && (!foundJackpot || !foundRollover); attempt++ {
			var h chainhash.Hash
			h[0] = byte(attempt)
			h[1] = byte(attempt >> 8)
			r := RunDraw(stakes, CalcTargetHash(h, drawHeight))
			var distributed int64
			for _, w := range r.Winners {
				distributed += w.Prize
			}
			distributed += r.Rollover
			if distributed != totalPool {
				t.Fatalf("value not conserved: pool=%d distributed=%d", totalPool, distributed)
			}
			if r.JackpotWon {
				foundJackpot = true
			} else {
				foundRollover = true
			}
		}
		if !foundJackpot {
			t.Error("no jackpot found in 200k attempts")
		}
		if !foundRollover {
			t.Error("no rollover found in 200k attempts")
		}
	})

	t.Run("tape_rollover_when_extended_pos_out_of_range", func(t *testing.T) {
		// 1 token staked; tape = WinMultiplier positions.
		// Jackpot zone is position 0 only; 9/10 draws must roll over.
		stakes := []StakeEntry{
			{WalletHash: [20]byte{0x01}, TokenCount: 1, TotalValue: satoshi},
		}
		var found bool
		for attempt := 0; attempt < 50; attempt++ {
			var h chainhash.Hash
			h[0] = byte(attempt + 1) // avoid zero which maps to pos=0
			r := RunDraw(stakes, CalcTargetHash(h, drawHeight))
			if !r.JackpotWon && r.Rollover == satoshi {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("rollover path not found in 50 attempts")
		}
	})

	t.Run("tape_jackpot_when_extended_pos_in_range", func(t *testing.T) {
		// Find a hash where extendedPos lands in [0, totalRealTokens) → jackpot.
		stakes := []StakeEntry{
			{WalletHash: [20]byte{0x01}, TokenCount: 1, TotalValue: satoshi},
		}
		var found bool
		for attempt := 0; attempt < 200000; attempt++ {
			var h chainhash.Hash
			h[0] = byte(attempt)
			h[1] = byte(attempt >> 8)
			r := RunDraw(stakes, CalcTargetHash(h, drawHeight))
			if r.JackpotWon && len(r.Winners) == 1 && r.Winners[0].Prize == satoshi {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("jackpot path not found in 200k attempts")
		}
	})

	t.Run("jackpot_pool_excluded_from_winner_selection", func(t *testing.T) {
		// Jackpot pool UTXO must boost prize but never appear as a winner.
		stakes := []StakeEntry{
			{WalletHash: [20]byte{0x01}, TokenCount: 1, TotalValue: satoshi},
			{WalletHash: [20]byte{0x02}, TokenCount: 1, TotalValue: satoshi},
			{WalletHash: JackpotPoolWalletHash, TokenCount: 0, TotalValue: 5 * satoshi},
		}
		wantPool := int64(7 * satoshi)
		var foundJackpot bool
		for attempt := 0; attempt < 200000; attempt++ {
			var h chainhash.Hash
			h[0] = byte(attempt)
			h[1] = byte(attempt >> 8)
			r := RunDraw(stakes, CalcTargetHash(h, drawHeight))
			var distributed int64
			for _, w := range r.Winners {
				distributed += w.Prize
				if w.WalletHash == JackpotPoolWalletHash {
					t.Fatal("jackpot pool address must not be a winner")
				}
			}
			distributed += r.Rollover
			if distributed != wantPool {
				t.Fatalf("pool not conserved: want=%d got=%d", wantPool, distributed)
			}
			if r.JackpotWon {
				foundJackpot = true
				break
			}
		}
		if !foundJackpot {
			t.Skip("no jackpot found in 200k attempts — skipping winner assertion")
		}
	})

	t.Run("single_winner_takes_full_pool", func(t *testing.T) {
		// Mod selection maps to exactly one position — no ties possible.
		const tokens = int64(100)
		stakes := make([]StakeEntry, 10)
		totalPool := int64(0)
		for i := range stakes {
			stakes[i] = StakeEntry{
				WalletHash: [20]byte{byte(i + 1)},
				TokenCount: tokens,
				TotalValue: satoshi * tokens,
			}
			totalPool += satoshi * tokens
		}
		for attempt := 0; attempt < 200000; attempt++ {
			var h chainhash.Hash
			h[0] = byte(attempt)
			h[1] = byte(attempt >> 8)
			r := RunDraw(stakes, CalcTargetHash(h, drawHeight))
			if !r.JackpotWon {
				continue
			}
			if len(r.Winners) != 1 {
				t.Fatalf("expected exactly 1 winner, got %d", len(r.Winners))
			}
			if r.Winners[0].Prize != totalPool {
				t.Fatalf("winner prize %d != pool %d", r.Winners[0].Prize, totalPool)
			}
			return
		}
		t.Skip("no jackpot in 200k attempts")
	})
}
