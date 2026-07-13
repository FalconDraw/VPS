package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"

	"github.com/btcsuite/btcd/blockchain"
)

// RFC 3526 MODP Group 14 2048-bit prime — same constant as chaincfg.VDFPrime.
const vdfPrimeHex = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD1" +
	"29024E088A67CC74020BBEA63B139B22514A08798E3404DD" +
	"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245" +
	"E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED" +
	"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3D" +
	"C2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F" +
	"83655D23DCA3AD961C62F356208552BB9ED529077096966D" +
	"670C354E4ABC9804F1746C08CA18217C32905E462E36CE3B" +
	"E39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9" +
	"DE2BCBF6955817183995497CEA956AE515D2261898FA0510" +
	"15728E5A8AACAA68FFFFFFFFFFFFFFFF"

type rpcReq struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
	ID      int           `json:"id"`
}

type drawResult struct {
	DrawSequence   int    `json:"drawsequence"`
	DrawnBlockHash string `json:"drawnblockhash"`
	Settled        bool   `json:"settled"`
	TotalTokens    int    `json:"totaltokens"`
	VDFT           uint64 `json:"vdft"`
	VDFOutput      string `json:"vdfoutput"`
	JackpotWon     bool   `json:"jackpotwon"`
	WinningPos     int64  `json:"winningpos"`
}

func rpcCall(method string, params []interface{}) (json.RawMessage, error) {
	body, _ := json.Marshal(rpcReq{"1.0", method, params, 1})
	resp, err := http.Post("http://falconrpc:OyuXOboW7Ixs67fp1FOssfTGmoPEerEc@127.0.0.1:8334",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  interface{}     `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Error != nil {
		return nil, fmt.Errorf("%v", out.Error)
	}
	return out.Result, nil
}

func main() {
	prime, _ := new(big.Int).SetString(vdfPrimeHex, 16)
	vdfParams := blockchain.NewVDFParams(prime)

	raw, err := rpcCall("getblockcount", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "getblockcount:", err)
		os.Exit(1)
	}
	var tipHeight int
	json.Unmarshal(raw, &tipHeight)

	pass, fail := 0, 0

	for dh := 36; dh <= tipHeight; dh += 36 {
		raw, err := rpcCall("getdrawresult", []interface{}{dh})
		if err != nil {
			continue // no participants
		}
		var dr drawResult
		if err := json.Unmarshal(raw, &dr); err != nil || !dr.Settled || dr.TotalTokens == 0 {
			continue
		}

		// Block hash in RPC is display order (byte-reversed) — reverse back to internal order.
		hashBytes, err := hex.DecodeString(dr.DrawnBlockHash)
		if err != nil || len(hashBytes) != 32 {
			fmt.Printf("Seq %2d | Draw %6d | SKIP bad hash\n", dr.DrawSequence, dh)
			continue
		}
		var seed [32]byte
		for i := 0; i < 32; i++ {
			seed[i] = hashBytes[31-i]
		}

		output, ok := new(big.Int).SetString(dr.VDFOutput, 16)
		if !ok {
			fmt.Printf("Seq %2d | Draw %6d | SKIP bad vdf output\n", dr.DrawSequence, dh)
			continue
		}

		valid := blockchain.SlothVerify(seed, output, dr.VDFT, vdfParams)
		status := "OK"
		if !valid {
			status = "FAIL"
			fail++
		} else {
			pass++
		}
		fmt.Printf("Seq %2d | Draw %6d | T=%-5d | pos=%d/%d | jackpot=%v | VDF %s\n",
			dr.DrawSequence, dh, dr.VDFT, dr.WinningPos, dr.TotalTokens*10, dr.JackpotWon, status)
	}

	fmt.Printf("\n%d verified OK, %d FAILED\n", pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}
