package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	address "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/database"
	wire "github.com/btcsuite/btcd/wire/v2"
)

// handleGetDrawInfo implements the getdrawinfo command. It returns the current
// staking state for a draw — prize pool, tier structure, per-staker odds, and
// whether the draw has already been settled.
func handleGetDrawInfo(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	c := cmd.(*btcjson.GetDrawInfoCmd)
	drawHeight := c.DrawHeight

	if drawHeight == 0 || int32(drawHeight)%blockchain.DrawInterval != 0 {
		return nil, &btcjson.RPCError{
			Code: btcjson.ErrRPCInvalidParameter,
			Message: fmt.Sprintf("drawheight %d is not a valid draw block height "+
				"(must be a nonzero multiple of %d)", drawHeight, blockchain.DrawInterval),
		}
	}

	stakes, err := s.cfg.Chain.FetchDrawStakes(drawHeight)
	if err != nil {
		return nil, internalRPCError(err.Error(), "fetching draw stakes")
	}

	// Aggregate real stakers by wallet hash (one wallet may have multiple TXs).
	type walletAgg struct{ tokens, totalAtoms int64 }
	aggMap := make(map[[20]byte]*walletAgg)
	var aggOrder [][20]byte
	var stakerAtoms, realAtoms, totalRealTokens int64
	for _, st := range stakes {
		stakerAtoms += st.TotalAtoms
		if st.WalletHash == blockchain.JackpotPoolWalletHash {
			continue
		}
		realAtoms += st.TotalAtoms
		totalRealTokens += st.Tokens
		if a, ok := aggMap[st.WalletHash]; ok {
			a.tokens += st.Tokens
			a.totalAtoms += st.TotalAtoms
		} else {
			aggMap[st.WalletHash] = &walletAgg{tokens: st.Tokens, totalAtoms: st.TotalAtoms}
			aggOrder = append(aggOrder, st.WalletHash)
		}
	}
	numStakers := len(aggOrder)

	stakerInfos := make([]btcjson.DrawStakerInfo, 0, numStakers)
	for _, wh := range aggOrder {
		a := aggMap[wh]
		stakerInfos = append(stakerInfos, btcjson.DrawStakerInfo{
			WalletHash: hex.EncodeToString(wh[:]),
			Tokens:     a.tokens,
			FDRW:       btcutil.Amount(a.totalAtoms).ToBTC(),
		})
	}

	// Full jackpot = pool balance (staker amounts + rolled-over amounts from prior draws).
	// Use ReconstructPoolBalance for unsettled draws so future/rollover draws show
	// the full accumulated pool even before absorption has run.
	_, _, resultFound, _ := s.cfg.Chain.FetchDrawResultMeta(drawHeight)
	var poolBalance int64
	if resultFound {
		poolBalance = s.cfg.Chain.FetchPoolBalance(drawHeight)
	} else {
		poolBalance = s.cfg.Chain.ReconstructPoolBalance(drawHeight)
	}
	jackpotPoolAtoms := poolBalance - stakerAtoms
	if jackpotPoolAtoms < 0 {
		jackpotPoolAtoms = 0
	}

	best := s.cfg.Chain.BestSnapshot().Height
	drawLocked := best >= int32(drawHeight) // staking window closed once draw block connects

	// Blocks until staking window closes: window closes at drawHeight-1.
	var blocksUntilDraw int32
	if !drawLocked {
		blocksUntilDraw = int32(drawHeight) - 1 - best
		if blocksUntilDraw < 0 {
			blocksUntilDraw = 0
		}
	}

	prevDraw := drawHeight - uint32(blockchain.DrawInterval)
	drawSeq := 1
	if prevDraw > 0 {
		if n, err2 := s.cfg.Chain.CountSettledDraws(prevDraw); err2 == nil {
			drawSeq = n + 1
		}
	}

	// VDF status.
	settlementHeightVDF, vdfT, vdfOut, vdfSettled := s.cfg.Chain.FetchVDFResult(drawHeight)
	vdfComputing := false
	var vdfProgressPct uint32
	var vdfSecondsRemaining int64
	var vdfElapsedSecs int64
	var vdfActualSecs int64
	var vdfOutputHex string
	drawBlockPassed := best >= int32(drawHeight)

	if vdfSettled {
		// Encode VDF output as hex.
		out256 := make([]byte, 256)
		vdfOut.FillBytes(out256)
		vdfOutputHex = hex.EncodeToString(out256)
		vdfProgressPct = 100
	} else if drawBlockPassed && numStakers > 0 {
		// VDF is computing or not yet started (only runs when real stakers exist).
		pending := s.cfg.Chain.GetPendingVDFResult()
		if pending != nil && pending.DrawHeight == drawHeight {
			// Computation complete but block not yet mined.
			vdfComputing = false
			vdfActualSecs = pending.ComputeSecs
			vdfProgressPct = 100
		} else {
			vdfComputing = true
			vdfProgressPct = s.cfg.Chain.GetVDFProgressPct()
			// Estimate remaining time using the calibrated T for this draw.
			// Cosmetic only now — the progress bar itself uses vdfProgressPct,
			// the real step count reported by the running computation.
			msPerStep := s.cfg.Chain.GetVDFMsPerStep()
			estimatedT := s.cfg.Chain.CalcNextVDFT(drawHeight)
			estimatedTotalMs := int64(float64(estimatedT) * msPerStep)
			startTime := s.cfg.Chain.GetVDFStartTime()
			if !startTime.IsZero() {
				elapsed := time.Since(startTime).Milliseconds()
				vdfElapsedSecs = elapsed / 1000
				remaining := estimatedTotalMs - elapsed
				if remaining < 0 {
					remaining = 0
				}
				vdfSecondsRemaining = remaining / 1000
			} else {
				vdfSecondsRemaining = estimatedTotalMs / 1000
			}
		}
	}

	var settlementHeightOut uint32
	if vdfSettled {
		settlementHeightOut = uint32(settlementHeightVDF)
	}

	// CurrentVDFT: the T currently in use — pending result's T if computing,
	// settled result's T if done, otherwise the calibrated target for this draw.
	currentVDFT := s.cfg.Chain.CalcNextVDFT(drawHeight)
	if pending := s.cfg.Chain.GetPendingVDFResult(); pending != nil {
		currentVDFT = pending.T
	} else if vdfSettled && vdfT > 0 {
		currentVDFT = vdfT
	}

	return &btcjson.GetDrawInfoResult{
		DrawHeight:          drawHeight,
		DrawSequence:        drawSeq,
		SettlementHeight:    settlementHeightOut,
		Settled:             vdfSettled,
		BlocksUntilDraw:     blocksUntilDraw,
		WinProbabilityPct:   float64(100) / float64(blockchain.WinMultiplier),
		TotalPoolFDRW:       btcutil.Amount(poolBalance).ToBTC(),
		RealPoolFDRW:        btcutil.Amount(realAtoms).ToBTC(),
		JackpotPoolFDRW:     btcutil.Amount(jackpotPoolAtoms).ToBTC(),
		NumStakers:          numStakers,
		TotalTokens:         totalRealTokens,
		Stakers:             stakerInfos,
		VDFComputing:        vdfComputing,
		VDFProgressPct:      vdfProgressPct,
		VDFSecondsRemaining: vdfSecondsRemaining,
		VDFElapsedSecs:      vdfElapsedSecs,
		VDFActualSecs:       vdfActualSecs,
		VDFDone:             vdfSettled || (s.cfg.Chain.GetPendingVDFResult() != nil && s.cfg.Chain.GetPendingVDFResult().DrawHeight == drawHeight),
		VDFT:                vdfT,
		VDFOutput:           vdfOutputHex,
		CurrentVDFT:         currentVDFT,
	}, nil
}

// drawResultNoParticipants is a sentinel stored in drawResultCache for draws
// that settled as jackpot-rollover-only (no real stakers). A non-nil pointer
// that compares unequal to any real result is all we need.
var drawResultNoParticipants = new(btcjson.GetDrawResultResult)

// handleGetDrawResult implements the getdrawresult command. It retrieves the
// settlement tx from the chain and parses winners and rollover info.
// Results are cached permanently in s.drawResultCache because settled draw
// results are immutable. Both real results and rollover-only draws (sentinel)
// are cached so repeat calls never re-read the same block from disk.
func handleGetDrawResult(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	c := cmd.(*btcjson.GetDrawResultCmd)
	drawHeight := c.DrawHeight

	if drawHeight == 0 || int32(drawHeight)%blockchain.DrawInterval != 0 {
		return nil, &btcjson.RPCError{
			Code: btcjson.ErrRPCInvalidParameter,
			Message: fmt.Sprintf("drawheight %d is not a valid draw block height "+
				"(must be a nonzero multiple of %d)", drawHeight, blockchain.DrawInterval),
		}
	}

	if v, ok := s.drawResultCache.Load(drawHeight); ok {
		res := v.(*btcjson.GetDrawResultResult)
		if res == drawResultNoParticipants {
			return nil, &btcjson.RPCError{
				Code:    btcjson.ErrRPCBlockNotFound,
				Message: fmt.Sprintf("draw %d had no participants (jackpot rollover only)", drawHeight),
			}
		}
		return res, nil
	}

	// Fetch VDF result — required for a fully settled draw.
	vdfSettlementHeight, vdfT, vdfOutput, vdfSettled := s.cfg.Chain.FetchVDFResult(drawHeight)
	if !vdfSettled {
		// Draw block has passed but VDF not yet settled. Return partial data
		// so the UI can show "Computing VDF" in the draw history rather than
		// silently omitting the draw.
		stakes, _, _ := s.cfg.Chain.FetchDrawAllStakers(drawHeight)
		var totalTokens int64
		for _, st := range stakes {
			totalTokens += st.Tokens
		}
		if totalTokens == 0 {
			return nil, &btcjson.RPCError{
				Code:    btcjson.ErrRPCBlockNotFound,
				Message: fmt.Sprintf("draw %d had no participants", drawHeight),
			}
		}
		// Count settled draws BEFORE this one and add 1 — same formula as
		// getdrawinfo — so the partial result carries the correct draw number.
		prevDraw := drawHeight - uint32(blockchain.DrawInterval)
		prevCount, _ := s.cfg.Chain.CountSettledDraws(prevDraw)
		drawSeq := prevCount + 1
		totalPoolAtoms := s.cfg.Chain.ReconstructPoolBalance(drawHeight)
		var drawBlockHashStr string
		if h, err := s.cfg.Chain.BlockHashByHeight(int32(drawHeight)); err == nil {
			drawBlockHashStr = h.String()
		}
		return &btcjson.GetDrawResultResult{
			DrawHeight:    drawHeight,
			DrawSequence:  drawSeq,
			DrawBlockHash: drawBlockHashStr,
			TotalPoolFDRW: btcutil.Amount(totalPoolAtoms).ToBTC(),
			Settled:       false,
			TotalTokens:   totalTokens,
			Winners:       []btcjson.DrawWinnerInfo{},
		}, nil
	}

	// Build VDF output hex.
	out256 := make([]byte, 256)
	vdfOutput.FillBytes(out256)
	vdfOutputHex := hex.EncodeToString(out256)

	settleTx, drawBlockHash, err := s.cfg.Chain.FetchDrawSettlement(drawHeight)
	if err != nil {
		return nil, internalRPCError(err.Error(), "fetching draw settlement")
	}

	// No settlement tx → rollover draw (no stakers or zero balance).
	if settleTx == nil {
		jackpotWon, _, found, metaErr := s.cfg.Chain.FetchDrawResultMeta(drawHeight)
		if metaErr != nil || (!found) {
			return nil, &btcjson.RPCError{
				Code:    btcjson.ErrRPCBlockNotFound,
				Message: fmt.Sprintf("draw %d VDF settled but draw result missing", drawHeight),
			}
		}
		if found && jackpotWon {
			return nil, &btcjson.RPCError{
				Code:    btcjson.ErrRPCBlockNotFound,
				Message: fmt.Sprintf("draw %d: jackpot won but no settlement tx (anomalous)", drawHeight),
			}
		}

		stakes, _, _ := s.cfg.Chain.FetchDrawAllStakers(drawHeight)
		var totalTokens int64
		for _, st := range stakes {
			totalTokens += st.Tokens
		}
		if totalTokens == 0 {
			s.drawResultCache.Store(drawHeight, drawResultNoParticipants)
			return nil, &btcjson.RPCError{
				Code:    btcjson.ErrRPCBlockNotFound,
				Message: fmt.Sprintf("draw %d had no participants (jackpot rollover only)", drawHeight),
			}
		}

		// Fetch actual draw block hash.
		if actualDrawHash, hashErr := s.cfg.Chain.BlockHashByHeight(int32(drawHeight)); hashErr == nil {
			drawBlockHash = *actualDrawHash
		}
		drawSeq, _ := s.cfg.Chain.CountSettledDraws(drawHeight)
		totalPoolAtoms := s.cfg.Chain.ReconstructPoolBalance(drawHeight)

		// Compute winning position using VDF seed.
		var winningPos int64
		if totalTokens > 0 {
			seed := blockchain.DrawSeed(drawBlockHash, vdfOutput)
			winningPos = blockchain.SelectWinnerPos(seed, totalTokens*blockchain.WinMultiplier)
		}

		result := &btcjson.GetDrawResultResult{
			DrawHeight:       drawHeight,
			DrawSequence:     drawSeq,
			DrawBlockHash:    drawBlockHash.String(),
			TotalPoolFDRW:    btcutil.Amount(totalPoolAtoms).ToBTC(),
			Settled:          true,
			JackpotWon:       false,
			RolloverFDRW:     btcutil.Amount(totalPoolAtoms).ToBTC(),
			WinningPos:       winningPos,
			TotalTokens:      totalTokens,
			NumWinners:       0,
			Winners:          []btcjson.DrawWinnerInfo{},
			SettlementTxID:   "",
			SettlementHeight: vdfSettlementHeight,
			VDFT:             vdfT,
			VDFOutput:        vdfOutputHex,
		}
		s.drawResultCache.Store(drawHeight, result)
		return result, nil
	}

	_, hasReal, liveErr := s.cfg.Chain.FetchDrawLiveResult(drawHeight)
	if liveErr == nil && !hasReal {
		s.drawResultCache.Store(drawHeight, drawResultNoParticipants)
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCBlockNotFound,
			Message: fmt.Sprintf("draw %d had no participants (jackpot rollover only)", drawHeight),
		}
	}

	outs := settleTx.MsgTx().TxOut
	jackpotWon := len(outs) >= 2

	var totalAtoms, rolloverAtoms int64
	winners := make([]btcjson.DrawWinnerInfo, 0, 1)
	if jackpotWon {
		out := outs[1]
		totalAtoms = out.Value
		var walletHash [20]byte
		if len(out.PkScript) == 22 {
			copy(walletHash[:], out.PkScript[2:22])
		}
		winners = append(winners, btcjson.DrawWinnerInfo{
			WalletHash: hex.EncodeToString(walletHash[:]),
			PrizeFDRW:  btcutil.Amount(out.Value).ToBTC(),
		})
	} else {
		totalAtoms = s.cfg.Chain.ReconstructPoolBalance(drawHeight)
		rolloverAtoms = totalAtoms
	}

	drawSeq, _ := s.cfg.Chain.CountSettledDraws(drawHeight)

	var winningPos, totalTokens int64
	stakes, _, _ := s.cfg.Chain.FetchDrawAllStakers(drawHeight)
	for _, st := range stakes {
		totalTokens += st.Tokens
	}
	if totalTokens > 0 {
		seed := blockchain.DrawSeed(drawBlockHash, vdfOutput)
		winningPos = blockchain.SelectWinnerPos(seed, totalTokens*blockchain.WinMultiplier)
	}

	result := &btcjson.GetDrawResultResult{
		DrawHeight:       drawHeight,
		DrawSequence:     drawSeq,
		DrawBlockHash:    drawBlockHash.String(),
		TotalPoolFDRW:    btcutil.Amount(totalAtoms).ToBTC(),
		Settled:          true,
		JackpotWon:       jackpotWon,
		RolloverFDRW:     btcutil.Amount(rolloverAtoms).ToBTC(),
		WinningPos:       winningPos,
		TotalTokens:      totalTokens,
		NumWinners:       len(winners),
		Winners:          winners,
		SettlementTxID:   settleTx.Hash().String(),
		SettlementHeight: vdfSettlementHeight,
		VDFT:             vdfT,
		VDFOutput:        vdfOutputHex,
	}
	s.drawResultCache.Store(drawHeight, result)
	return result, nil
}

// handleGetDrawSizeInfo implements the getdrawsizeinfo command. It estimates
// how large the settlement block will be given the current staking UTXO count,
// so operators and participants can watch the block grow as the draw approaches.
func handleGetDrawSizeInfo(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	c := cmd.(*btcjson.GetDrawSizeInfoCmd)

	best := s.cfg.Chain.BestSnapshot()

	// Resolve draw height: use the supplied value or default to next draw.
	var drawHeight uint32
	if c.DrawHeight != nil {
		drawHeight = *c.DrawHeight
		if drawHeight == 0 || int32(drawHeight)%blockchain.DrawInterval != 0 {
			return nil, &btcjson.RPCError{
				Code: btcjson.ErrRPCInvalidParameter,
				Message: fmt.Sprintf("drawheight %d is not a valid draw block height "+
					"(must be a nonzero multiple of %d)", drawHeight, blockchain.DrawInterval),
			}
		}
	} else {
		drawHeight = uint32(blockchain.NextDrawHeight(best.Height))
	}

	blocksUntilDraw := int32(drawHeight) - best.Height

	stakes, err := s.cfg.Chain.FetchDrawStakes(drawHeight)
	if err != nil {
		return nil, internalRPCError(err.Error(), "fetching draw stakes")
	}

	// Count unique wallets and total stake entries.
	seenWallets := make(map[[20]byte]struct{})
	numUTXOs := 0
	for _, st := range stakes {
		if st.WalletHash != blockchain.JackpotPoolWalletHash {
			seenWallets[st.WalletHash] = struct{}{}
		}
		numUTXOs++
	}
	numWallets := len(seenWallets)

	// Estimate settlement tx size:
	//   10 bytes overhead (version, locktime, varint counts)
	//   41 bytes per input (outpoint 36 + scriptLen 1 + sequence 4)
	//   max 11 outputs × 34 bytes (8 value + 1 scriptLen + 25 P2PKH or 28 staking)
	const (
		txOverhead     = 10
		bytesPerInput  = 41
		bytesPerOutput = 34
		maxOutputs     = 11 // 1 marker + 10 winner tiers + 1 rollover
	)
	estBytes := int64(txOverhead) + int64(numUTXOs)*bytesPerInput + int64(maxOutputs)*bytesPerOutput
	// Settlement tx has no witnesses; weight = baseSize × 4.
	estWeight := estBytes * 4

	maxWeight := int64(blockchain.MaxVDFBlockWeight)
	usedPct := float64(estWeight) / float64(maxWeight) * 100.0

	return &btcjson.GetDrawSizeInfoResult{
		DrawHeight:          drawHeight,
		BlocksUntilDraw:     blocksUntilDraw,
		NumStakingUTXOs:     numUTXOs,
		NumWallets:          numWallets,
		EstSettlementBytes:  estBytes,
		EstSettlementWeight: estWeight,
		MaxSettlementWeight: maxWeight,
		WeightUsedPct:       usedPct,
	}, nil
}

// handleGetMempoolStakes implements the getmempoolstakes command. It scans the
// mempool for unconfirmed transactions that contain staking outputs targeting
// the given draw height, returning each as a pending stake entry.
func handleGetMempoolStakes(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	c := cmd.(*btcjson.GetMempoolStakesCmd)
	drawHeight := c.DrawHeight

	if drawHeight == 0 || int32(drawHeight)%blockchain.DrawInterval != 0 {
		return nil, &btcjson.RPCError{
			Code: btcjson.ErrRPCInvalidParameter,
			Message: fmt.Sprintf("drawheight %d is not a valid draw block height "+
				"(must be a nonzero multiple of %d)", drawHeight, blockchain.DrawInterval),
		}
	}

	var stakes []btcjson.MempoolStakeEntry
	for _, txDesc := range s.cfg.TxMemPool.TxDescs() {
		tx := txDesc.Tx.MsgTx()
		if tx.Version != wire.TxVersionPoolStake {
			continue
		}
		_, script := blockchain.FindPoolStakingOpReturn(tx)
		if script == nil {
			continue
		}
		wh, amt, dh, err := blockchain.ExtractPoolStakingData(script)
		if err != nil || dh != drawHeight {
			continue
		}
		// Find the vout index of the OP_RETURN for reporting.
		var opReturnVout uint32
		for vout, out := range tx.TxOut {
			if blockchain.IsPoolStakingOpReturn(out.PkScript) {
				opReturnVout = uint32(vout)
				break
			}
		}
		stakes = append(stakes, btcjson.MempoolStakeEntry{
			TxID:       txDesc.Tx.Hash().String(),
			Vout:       opReturnVout,
			WalletHash: hex.EncodeToString(wh[:]),
			FDRW:       btcutil.Amount(amt).ToBTC(),
		})
	}
	if stakes == nil {
		stakes = []btcjson.MempoolStakeEntry{}
	}

	return &btcjson.GetMempoolStakesResult{
		DrawHeight: drawHeight,
		Stakes:     stakes,
	}, nil
}

// handleGetDrawStakers implements the getdrawstakers command. Returns all
// wallets that staked in a draw, for both settled and upcoming draws.
func handleGetDrawStakers(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	c := cmd.(*btcjson.GetDrawStakersCmd)
	drawHeight := c.DrawHeight
	if drawHeight == 0 || int32(drawHeight)%blockchain.DrawInterval != 0 {
		return nil, &btcjson.RPCError{
			Code: btcjson.ErrRPCInvalidParameter,
			Message: fmt.Sprintf("drawheight %d is not a valid draw block height "+
				"(must be a nonzero multiple of %d)", drawHeight, blockchain.DrawInterval),
		}
	}
	stakes, settled, err := s.cfg.Chain.FetchDrawAllStakers(drawHeight)
	if err != nil {
		return nil, internalRPCError(err.Error(), "fetching draw stakers")
	}
	entries := make([]btcjson.DrawStakerEntry, 0, len(stakes))
	for _, st := range stakes {
		entries = append(entries, btcjson.DrawStakerEntry{
			WalletHash: hex.EncodeToString(st.WalletHash[:]),
			FDRW:       btcutil.Amount(st.TotalAtoms).ToBTC(),
			Tokens:     st.Tokens,
		})
	}
	return &btcjson.GetDrawStakersResult{
		DrawHeight: drawHeight,
		Settled:    settled,
		Stakers:    entries,
	}, nil
}

// handleGetStakerStats implements the getstakerstats command. Returns
// cumulative staker participation counts: the most recently closed draw, the
// last 10 closed draws (deduplicated), and the all-time unique wallet total.
func handleGetStakerStats(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	height := s.cfg.Chain.BestSnapshot().Height
	stats := s.cfg.Chain.FetchStakerStats(uint32(height))
	return &btcjson.GetStakerStatsResult{
		LastDrawStakers:    stats.LastDrawStakers,
		Last10DrawsStakers: stats.Last10DrawsStakers,
		TotalStakers:       stats.TotalStakers,
	}, nil
}

// handleGetDrawTickets implements the getdrawtickets command. For a settled draw
// it returns the wallet's token range [rangeStart, rangeEnd), the draw entropy
// hash, and the winning position so verifiers can confirm the draw independently.
func handleGetDrawTickets(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	c := cmd.(*btcjson.GetDrawTicketsCmd)
	drawHeight := c.DrawHeight

	if drawHeight == 0 || int32(drawHeight)%blockchain.DrawInterval != 0 {
		return nil, &btcjson.RPCError{
			Code: btcjson.ErrRPCInvalidParameter,
			Message: fmt.Sprintf("drawheight %d is not a valid draw block height "+
				"(must be a nonzero multiple of %d)", drawHeight, blockchain.DrawInterval),
		}
	}

	hashBytes, err := hex.DecodeString(c.WalletHash)
	if err != nil || len(hashBytes) != 20 {
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCInvalidParameter,
			Message: "wallethash must be exactly 20 bytes (40 hex characters)",
		}
	}
	var walletHash [20]byte
	copy(walletHash[:], hashBytes)

	// Draw must be VDF-settled before ticket verification is possible.
	_, _, vdfOutput, vdfSettled := s.cfg.Chain.FetchVDFResult(drawHeight)
	if !vdfSettled {
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCBlockNotFound,
			Message: fmt.Sprintf("draw %d has not been settled yet (awaiting VDF result)", drawHeight),
		}
	}

	// Fetch draw block hash from chain.
	drawBlockHashPtr, err := s.cfg.Chain.BlockHashByHeight(int32(drawHeight))
	if err != nil {
		return nil, internalRPCError(err.Error(), "fetching draw block hash")
	}
	drawBlockHash := *drawBlockHashPtr

	tokenCount, err := s.cfg.Chain.FetchWalletTokensForDraw(walletHash, drawHeight)
	if err != nil {
		return nil, internalRPCError(err.Error(), "fetching wallet token count")
	}
	if tokenCount == 0 {
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCInvalidParameter,
			Message: fmt.Sprintf("wallet %s did not stake in draw %d", c.WalletHash, drawHeight),
		}
	}

	// VDF-derived seed: SHA256d(drawBlockHash || vdfOutput[256 bytes]).
	seed := blockchain.DrawSeed(drawBlockHash, vdfOutput)

	// Fetch all stakers to compute this wallet's sorted position range.
	allStakers, _, err2 := s.cfg.Chain.FetchDrawAllStakers(drawHeight)
	if err2 != nil {
		return nil, internalRPCError(err2.Error(), "fetching draw stakers")
	}
	var rangeStart, totalTokens int64
	for _, st := range allStakers {
		if st.WalletHash == walletHash {
			rangeStart = totalTokens
		}
		totalTokens += st.Tokens
	}

	// Extended position: seed mod (WinMultiplier × totalTokens).
	// Jackpot if extendedPos < totalTokens; winning wallet determined by binary search.
	var extendedPos int64
	if totalTokens > 0 {
		extendedPos = blockchain.SelectWinnerPos(seed, totalTokens*blockchain.WinMultiplier)
	}
	jackpotZone := extendedPos < totalTokens

	return &btcjson.GetDrawTicketsResult{
		WalletHash:    c.WalletHash,
		DrawHeight:    drawHeight,
		DrawBlockHash: drawBlockHash.String(),
		EntropyHash:   hex.EncodeToString(seed[:]),
		TotalTokens:   totalTokens,
		RangeStart:    rangeStart,
		RangeEnd:      rangeStart + tokenCount,
		WinningPos:    extendedPos,
		Won:           jackpotZone && extendedPos >= rangeStart && extendedPos < rangeStart+tokenCount,
	}, nil
}

// handleGetWalletStakeHistory returns all draws (past and future) in which the
// given wallet hash has pool stake entries, via a single sequential DB scan.
func handleGetWalletStakeHistory(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	c := cmd.(*btcjson.GetWalletStakeHistoryCmd)
	whBytes, err := hex.DecodeString(c.WalletHash)
	if err != nil || len(whBytes) != 20 {
		return nil, &btcjson.RPCError{
			Code:    btcjson.ErrRPCInvalidParameter,
			Message: "walletHash must be a 40-character hex string (20 bytes)",
		}
	}
	var walletHash [20]byte
	copy(walletHash[:], whBytes)

	rawEntries := s.cfg.Chain.FetchWalletStakeHistory(walletHash)
	entries := make([]btcjson.WalletStakeEntry, len(rawEntries))
	for i, e := range rawEntries {
		entries[i] = btcjson.WalletStakeEntry{
			DrawHeight: e.DrawHeight,
			FDRW:       float64(e.TotalAtoms) / float64(btcutil.SatoshiPerBitcoin),
		}
	}
	return &btcjson.GetWalletStakeHistoryResult{Entries: entries}, nil
}

// handleGetAddressIncoming returns confirmed transfers TO an address, excluding
// stake txs (v6), VDF txs (v7), coinbase, and change outputs (where the same
// address also appears in an input of the same tx).
func handleGetAddressIncoming(s *rpcServer, cmd interface{}, closeChan <-chan struct{}) (interface{}, error) {
	addrIndex := s.cfg.AddrIndex
	if addrIndex == nil {
		return nil, &btcjson.RPCError{Code: btcjson.ErrRPCMisc, Message: "Address index must be enabled (--addrindex)"}
	}
	txIndex := s.cfg.TxIndex
	if txIndex == nil {
		return nil, &btcjson.RPCError{Code: btcjson.ErrRPCMisc, Message: "Tx index must be enabled (--txindex)"}
	}

	c := cmd.(*btcjson.GetAddressIncomingCmd)
	addr, err := address.DecodeAddress(c.Address, s.cfg.ChainParams)
	if err != nil {
		return nil, &btcjson.RPCError{Code: btcjson.ErrRPCInvalidAddressOrKey, Message: "Invalid address: " + err.Error()}
	}
	addrHash := addr.ScriptAddress()

	// P2WPKH script: OP_0 OP_DATA20 <20-byte hash>
	wantScript := make([]byte, 22)
	wantScript[0] = 0x00
	wantScript[1] = 0x14
	copy(wantScript[2:], addrHash)

	type candidate struct {
		msgTx     wire.MsgTx
		blockHash *chainhash.Hash
		atoms     int64
	}
	var candidates []candidate

	// Pass 1: collect txs that have our address in an output.
	err = s.cfg.DB.View(func(dbTx database.Tx) error {
		const batchSize = uint32(2000)
		var skip uint32
		for {
			regions, _, err := addrIndex.TxRegionsForAddress(dbTx, addr, skip, batchSize, false)
			if err != nil {
				return err
			}
			if len(regions) == 0 {
				break
			}
			serializedTxns, err := dbTx.FetchBlockRegions(regions)
			if err != nil {
				skip += uint32(len(regions))
				if uint32(len(regions)) < batchSize {
					break
				}
				continue
			}
			for i, raw := range serializedTxns {
				var msgTx wire.MsgTx
				if err := msgTx.Deserialize(bytes.NewReader(raw)); err != nil {
					continue
				}
				// Skip coinbase, stake (v6), VDF result (v7).
				if blockchain.IsCoinBaseTx(&msgTx) || msgTx.Version == 6 || msgTx.Version == 7 {
					continue
				}
				// Sum all outputs to our address.
				var total int64
				for _, out := range msgTx.TxOut {
					if bytes.Equal(out.PkScript, wantScript) {
						total += out.Value
					}
				}
				if total == 0 {
					continue
				}
				hashCopy := new(chainhash.Hash)
				*hashCopy = *regions[i].Hash
				candidates = append(candidates, candidate{msgTx: msgTx, blockHash: hashCopy, atoms: total})
			}
			skip += uint32(len(regions))
			if uint32(len(regions)) < batchSize {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, internalRPCError(err.Error(), "address index scan")
	}

	// Pass 2: for each candidate, verify none of its inputs spend our address.
	var results []btcjson.AddressIncomingEntry
	for _, cand := range candidates {
		fromSelf := false
		for _, txIn := range cand.msgTx.TxIn {
			prevHash := &txIn.PreviousOutPoint.Hash
			region, err := txIndex.TxBlockRegion(prevHash)
			if err != nil || region == nil {
				continue
			}
			var prevBytes []byte
			if dbErr := s.cfg.DB.View(func(dbTx database.Tx) error {
				var e error
				prevBytes, e = dbTx.FetchBlockRegion(region)
				return e
			}); dbErr != nil {
				continue
			}
			var prevTx wire.MsgTx
			if err := prevTx.Deserialize(bytes.NewReader(prevBytes)); err != nil {
				continue
			}
			vout := txIn.PreviousOutPoint.Index
			if int(vout) < len(prevTx.TxOut) && bytes.Equal(prevTx.TxOut[vout].PkScript, wantScript) {
				fromSelf = true
				break
			}
		}
		if fromSelf {
			continue
		}

		height, err := s.cfg.Chain.BlockHeightByHash(cand.blockHash)
		if err != nil {
			height = 0
		}

		txHash := cand.msgTx.TxHash()
		results = append(results, btcjson.AddressIncomingEntry{
			Txid:        txHash.String(),
			Atoms:       cand.atoms,
			FDRW:        float64(cand.atoms) / float64(btcutil.SatoshiPerBitcoin),
			BlockHeight: height,
		})
	}

	// Newest first.
	sort.Slice(results, func(i, j int) bool {
		return results[i].BlockHeight > results[j].BlockHeight
	})

	return &btcjson.GetAddressIncomingResult{Entries: results}, nil
}
