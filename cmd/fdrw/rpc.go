package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type rpcClient struct {
	url      string
	user     string
	pass     string
	httpClient *http.Client
}

func newRPCClient(cfg *cliConfig) (*rpcClient, error) {
	var transport http.RoundTripper
	if cfg.NoTLS {
		transport = http.DefaultTransport
	} else {
		var tlsCfg *tls.Config
		if cfg.TLSSkipVerify {
			tlsCfg = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit user opt-in
		} else {
			pem, err := os.ReadFile(cfg.RPCCert)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf(
						"RPC certificate not found at %q\n"+
							"  • run btcd once to generate it, or\n"+
							"  • specify --rpccert=path, or\n"+
							"  • use --notls for local plaintext connections, or\n"+
							"  • use --tlsskipverify to skip verification (not recommended)",
						cfg.RPCCert)
				}
				return nil, fmt.Errorf("reading RPC cert %q: %w", cfg.RPCCert, err)
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tlsCfg = &tls.Config{RootCAs: pool}
		}
		transport = &http.Transport{TLSClientConfig: tlsCfg}
	}

	scheme := "https"
	if cfg.NoTLS {
		scheme = "http"
	}

	return &rpcClient{
		url:  fmt.Sprintf("%s://%s", scheme, cfg.RPCServer),
		user: cfg.RPCUser,
		pass: cfg.RPCPass,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}, nil
}

var rpcID int

func (c *rpcClient) call(method string, params ...interface{}) (json.RawMessage, error) {
	rpcID++
	body := map[string]interface{}{
		"jsonrpc": "1.0",
		"id":      rpcID,
		"method":  method,
		"params":  params,
	}
	if params == nil {
		body["params"] = []interface{}{}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.url, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RPC transport error: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("JSON decode: %w — body: %s", err, raw)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", result.Error.Code, result.Error.Message)
	}
	return result.Result, nil
}

func (c *rpcClient) getBlockCount() (int32, error) {
	raw, err := c.call("getblockcount")
	if err != nil {
		return 0, err
	}
	var n int32
	return n, json.Unmarshal(raw, &n)
}

func (c *rpcClient) getAddressUTXOs(addr string) ([]addrUTXO, error) {
	raw, err := c.call("getaddressutxos", addr)
	if err != nil {
		return nil, err
	}
	var utxos []addrUTXO
	return utxos, json.Unmarshal(raw, &utxos)
}

func (c *rpcClient) sendRawTx(hexTx string) (string, error) {
	raw, err := c.call("sendrawtransaction", hexTx, false)
	if err != nil {
		return "", err
	}
	var txid string
	return txid, json.Unmarshal(raw, &txid)
}


func (c *rpcClient) getDrawInfo(drawHeight uint32) (*drawInfoResult, error) {
	raw, err := c.call("getdrawinfo", drawHeight)
	if err != nil {
		return nil, err
	}
	var r drawInfoResult
	return &r, json.Unmarshal(raw, &r)
}

func (c *rpcClient) getDrawResult(drawHeight uint32) (*drawResultResult, error) {
	raw, err := c.call("getdrawresult", drawHeight)
	if err != nil {
		return nil, err
	}
	var r drawResultResult
	return &r, json.Unmarshal(raw, &r)
}

// Result types (mirrors btcjson structs — defined locally to avoid import cycle).

type addrUTXO struct {
	TxID          string  `json:"txid"`
	Vout          uint32  `json:"vout"`
	Atoms         int64   `json:"atoms"`
	Confirmations int32   `json:"confirmations"`
	Script        string  `json:"script"`
	IsCoinbase    bool    `json:"iscoinbase"`
	IsStaking     bool    `json:"isstaking"`
	DrawHeight    uint32  `json:"drawheight"`
}

type drawStakerInfo struct {
	WalletHash string  `json:"wallethash"`
	Tokens     int64   `json:"tokens"`
	FDRW       float64 `json:"fdrw"`
}

type drawInfoResult struct {
	DrawHeight       uint32           `json:"drawheight"`
	DrawSequence     int              `json:"drawsequence"`
	SettlementHeight uint32           `json:"settlementheight"`
	Settled          bool             `json:"settled"`
	TotalPoolFDRW    float64          `json:"totalpool"`
	RealPoolFDRW     float64          `json:"realpool"`
	JackpotPoolFDRW  float64          `json:"jackpotpool"`
	NumStakers       int              `json:"numstakers"`
	TotalTokens      int64            `json:"totaltokens"`
	Stakers          []drawStakerInfo `json:"stakers"`
}

type drawWinnerInfo struct {
	WalletHash string  `json:"wallethash"`
	PrizeFDRW  float64 `json:"prize"`
}

type drawResultResult struct {
	DrawHeight    uint32           `json:"drawheight"`
	DrawSequence  int              `json:"drawsequence"`
	DrawBlockHash string           `json:"drawnblockhash"`
	TotalPoolFDRW float64          `json:"totalpool"`
	JackpotWon    bool             `json:"jackpotwon"`
	RolloverFDRW  float64          `json:"rollover"`
	NumWinners    int              `json:"numwinners"`
	Winners       []drawWinnerInfo `json:"winners"`
	SettlementTxID string          `json:"settlementtxid"`
}

