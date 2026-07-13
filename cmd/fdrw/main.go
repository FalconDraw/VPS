// fdrw — FalconDraw wallet CLI
//
// Commands:
//
//	init          Generate a new wallet key in ~/.fdrw/wallet.key
//	info          Show wallet address and pubkey hash
//	balance       Show spendable balance
//	stake <amt>   Stake <amt> FDRW for the next draw
//	check-draw    Show current or specified draw state
//	draw-history  Show recent draw results
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	fdaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
)

// ── Config ────────────────────────────────────────────────────────────────────

type cliConfig struct {
	RPCServer     string
	RPCUser       string
	RPCPass       string
	RPCCert       string
	NoTLS         bool
	TLSSkipVerify bool
	KeyFile       string
	Net           *chaincfg.Params
}

func defaultConfig() *cliConfig {
	home, _ := os.UserHomeDir()
	return &cliConfig{
		RPCServer:     "localhost:8334",
		RPCUser:       "",
		RPCPass:       "",
		RPCCert:       filepath.Join(home, ".btcd", "rpc.cert"),
		NoTLS:         false,
		TLSSkipVerify: false,
		KeyFile:       filepath.Join(home, ".fdrw", "wallet.key"),
		Net:           &chaincfg.MainNetParams,
	}
}

// readBtcdConf parses ~/.btcd/btcd.conf and returns rpcuser and rpcpass values.
func readBtcdConf() (user, pass string) {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".btcd", "btcd.conf"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "rpcuser=") {
			user = strings.TrimPrefix(line, "rpcuser=")
		} else if strings.HasPrefix(line, "rpcpass=") {
			pass = strings.TrimPrefix(line, "rpcpass=")
		}
	}
	return
}

// loadConfig reads flags from os.Args. Flags: --rpcserver, --rpcuser,
// --rpcpass, --rpccert, --notls, --keyfile, --testnet.
// Returns cfg, remaining non-flag args.
func loadConfig(args []string) (*cliConfig, []string, error) {
	cfg := defaultConfig()
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			rest = append(rest, arg)
			continue
		}
		kv := strings.SplitN(strings.TrimPrefix(arg, "--"), "=", 2)
		key := kv[0]
		val := ""
		if len(kv) == 2 {
			val = kv[1]
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			i++
			val = args[i]
		}
		switch key {
		case "rpcserver":
			cfg.RPCServer = val
		case "rpcuser":
			cfg.RPCUser = val
		case "rpcpass":
			cfg.RPCPass = val
		case "rpccert":
			cfg.RPCCert = val
		case "notls":
			cfg.NoTLS = true
		case "tlsskipverify":
			cfg.TLSSkipVerify = true
		case "keyfile":
			cfg.KeyFile = val
		case "testnet":
			cfg.Net = &chaincfg.TestNet3Params
		case "regtest":
			cfg.Net = &chaincfg.RegressionNetParams
		}
	}

	// Auto-fill credentials from ~/.btcd/btcd.conf if not provided via flags.
	if cfg.RPCUser == "" || cfg.RPCPass == "" {
		u, p := readBtcdConf()
		if cfg.RPCUser == "" {
			cfg.RPCUser = u
		}
		if cfg.RPCPass == "" {
			cfg.RPCPass = p
		}
	}

	return cfg, rest, nil
}

// ── Wallet ────────────────────────────────────────────────────────────────────

func walletDir(keyFile string) string { return filepath.Dir(keyFile) }

func loadPrivKey(keyFile string) (*btcec.PrivateKey, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("key file not found — run 'fdrw init' first: %w", err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("invalid key file: must be 64 hex characters")
	}
	priv, _ := btcec.PrivKeyFromBytes(b)
	return priv, nil
}

func walletHashFromKey(priv *btcec.PrivateKey) [20]byte {
	h := fdaddr.Hash160(priv.PubKey().SerializeCompressed())
	var wh [20]byte
	copy(wh[:], h)
	return wh
}

func walletAddress(priv *btcec.PrivateKey, net *chaincfg.Params) (string, error) {
	h := fdaddr.Hash160(priv.PubKey().SerializeCompressed())
	addr, err := fdaddr.NewAddressWitnessPubKeyHash(h, net)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

// ── Commands ──────────────────────────────────────────────────────────────────

func cmdInit(cfg *cliConfig) error {
	if err := os.MkdirAll(walletDir(cfg.KeyFile), 0700); err != nil {
		return err
	}
	if _, err := os.Stat(cfg.KeyFile); err == nil {
		return fmt.Errorf("wallet key already exists at %s — delete it first to reinitialize", cfg.KeyFile)
	}
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return err
	}
	hexKey := hex.EncodeToString(priv.Serialize())
	if err := os.WriteFile(cfg.KeyFile, []byte(hexKey+"\n"), 0600); err != nil {
		return err
	}
	addr, _ := walletAddress(priv, cfg.Net)
	fmt.Printf("Wallet created.\nAddress:    %s\nKey file:   %s\n", addr, cfg.KeyFile)
	fmt.Println(`
WARNING: This is a hot wallet. The private key is stored unencrypted on this
server. Treat it as permanently compromisable — do not keep large balances
here. Jackpot prizes land in this wallet automatically; sweep them to cold
storage promptly using 'fdrw send'.

Back up your key file securely — it cannot be recovered if lost.`)
	return nil
}

func cmdInfo(cfg *cliConfig) error {
	priv, err := loadPrivKey(cfg.KeyFile)
	if err != nil {
		return err
	}
	addr, err := walletAddress(priv, cfg.Net)
	if err != nil {
		return err
	}
	wh := walletHashFromKey(priv)
	fmt.Printf("Address:     %s\nWallet hash: %x\nKey file:    %s\n", addr, wh, cfg.KeyFile)
	return nil
}

func cmdBalance(cfg *cliConfig, rpc *rpcClient) error {
	priv, err := loadPrivKey(cfg.KeyFile)
	if err != nil {
		return err
	}
	addr, err := walletAddress(priv, cfg.Net)
	if err != nil {
		return err
	}
	utxos, err := rpc.getAddressUTXOs(addr)
	if err != nil {
		return err
	}
	var spendable, staked int64
	for _, u := range utxos {
		if u.IsStaking {
			staked += u.Atoms
		} else if !u.IsCoinbase || u.Confirmations >= 100 {
			spendable += u.Atoms
		}
	}
	fmt.Printf("Address:    %s\nSpendable:  %s\nStaked:     %s\n",
		addr, btcutil.Amount(spendable), btcutil.Amount(staked))
	if spendable > 0 {
		fmt.Println("REMINDER: This is a hot wallet on a networked server. Sweep spendable funds to cold storage promptly.")
	}
	return nil
}


// cmdPoolStake submits a TxVersionPoolStake (v6/FDRP) staking transaction.
func cmdPoolStake(cfg *cliConfig, rpc *rpcClient, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: fdrw pool-stake <amount_fdrw>")
	}
	amtF, err := strconv.ParseFloat(args[0], 64)
	if err != nil || amtF <= 0 {
		return fmt.Errorf("invalid amount: %s", args[0])
	}
	tokenCount := uint32(amtF)
	if tokenCount == 0 || float64(tokenCount) != amtF {
		return fmt.Errorf("pool-stake amount must be a whole number of FDRW (got %s)", args[0])
	}
	stakeAtoms := int64(tokenCount) * btcutil.SatoshiPerBitcoin

	priv, err := loadPrivKey(cfg.KeyFile)
	if err != nil {
		return err
	}
	wh := walletHashFromKey(priv)
	addr, err := walletAddress(priv, cfg.Net)
	if err != nil {
		return err
	}

	height, err := rpc.getBlockCount()
	if err != nil {
		return fmt.Errorf("getblockcount: %w", err)
	}
	drawHeight := uint32(blockchain.NextDrawHeight(height))

	utxos, err := rpc.getAddressUTXOs(addr)
	if err != nil {
		return err
	}

	// Estimated vbytes: 10 overhead + 41 per input (no scriptSig) + 34 FDRP OP_RETURN + 31 change
	// + 25 witness vbytes per input (97 witness bytes × 0.25).
	estVbytes := 10 + (41+25) + 34 + 31
	changeScript := p2wpkhScript(wh[:])
	selected, change, err := selectUTXOs(utxos, stakeAtoms, estVbytes)
	if err != nil {
		return err
	}

	tx, err := buildPoolStakeTx(priv, wh, changeScript, selected, stakeAtoms, drawHeight, change)
	if err != nil {
		return err
	}

	hexTx, err := serializeTx(tx)
	if err != nil {
		return err
	}
	txid, err := rpc.sendRawTx(hexTx)
	if err != nil {
		return fmt.Errorf("broadcast failed: %w", err)
	}

	fmt.Printf("Pool-staked %d FDRW for draw %d\nTxID: %s\n", tokenCount, drawHeight, txid)
	return nil
}

func cmdCheckDraw(cfg *cliConfig, rpc *rpcClient, args []string) error {
	height, err := rpc.getBlockCount()
	if err != nil {
		return err
	}

	var drawHeight uint32
	if len(args) > 0 {
		h, err := strconv.ParseUint(args[0], 10, 32)
		if err != nil {
			return fmt.Errorf("invalid draw height: %s", args[0])
		}
		drawHeight = uint32(h)
	} else {
		drawHeight = uint32(blockchain.NextDrawHeight(height))
	}

	info, err := rpc.getDrawInfo(drawHeight)
	if err != nil {
		return fmt.Errorf("getdrawinfo: %w", err)
	}

	fmt.Printf("Draw #%d  (block %d, settlement %d)\n", info.DrawSequence, info.DrawHeight, info.SettlementHeight)
	fmt.Printf("Status:      %s\n", map[bool]string{true: "Settled", false: "Open"}[info.Settled])
	fmt.Printf("Pool:        %.8f FDRW (real: %.8f, jackpot: %.8f)\n",
		info.TotalPoolFDRW, info.RealPoolFDRW, info.JackpotPoolFDRW)
	fmt.Printf("Stakers:     %d  (%d tokens)\n", info.NumStakers, info.TotalTokens)
	blocksLeft := int32(info.DrawHeight) - height
	if blocksLeft > 0 {
		fmt.Printf("Locks in:    %d blocks (~%d minutes)\n", blocksLeft, blocksLeft*10)
	}

	if info.Settled {
		res, err := rpc.getDrawResult(drawHeight)
		if err != nil {
			return nil // draw may have had no participants
		}
		if res.JackpotWon {
			for _, w := range res.Winners {
				fmt.Printf("Winner:      %s  (%.8f FDRW)\n", w.WalletHash, w.PrizeFDRW)
			}
		} else {
			fmt.Printf("Result:      Rollover — %.8f FDRW carried forward\n", res.RolloverFDRW)
		}
	}
	return nil
}

func cmdSend(cfg *cliConfig, rpc *rpcClient, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: fdrw send <address> <amount_fdrw>")
	}
	recipientAddr, err := fdaddr.DecodeAddress(args[0], cfg.Net)
	if err != nil {
		return fmt.Errorf("invalid recipient address: %w", err)
	}
	recipientScript, err := txscript.PayToAddrScript(recipientAddr)
	if err != nil {
		return fmt.Errorf("building output script: %w", err)
	}
	amtF, err := strconv.ParseFloat(args[1], 64)
	if err != nil || amtF <= 0 {
		return fmt.Errorf("invalid amount: %s", args[1])
	}
	sendAtoms := int64(amtF * 1e8)
	if sendAtoms <= 0 {
		return fmt.Errorf("amount too small")
	}

	priv, err := loadPrivKey(cfg.KeyFile)
	if err != nil {
		return err
	}
	wh := walletHashFromKey(priv)
	addr, err := walletAddress(priv, cfg.Net)
	if err != nil {
		return err
	}

	utxos, err := rpc.getAddressUTXOs(addr)
	if err != nil {
		return err
	}

	tx, err := buildTransferTx(priv, wh, recipientScript, utxos, sendAtoms)
	if err != nil {
		return err
	}
	hexTx, err := serializeTx(tx)
	if err != nil {
		return err
	}
	txid, err := rpc.sendRawTx(hexTx)
	if err != nil {
		return fmt.Errorf("broadcast failed: %w", err)
	}
	fmt.Printf("Sent %.8f FDRW → %s\nTxID: %s\n", amtF, args[0], txid)
	return nil
}

func cmdDrawHistory(cfg *cliConfig, rpc *rpcClient, args []string) error {
	n := 10
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			n = v
		}
	}

	height, err := rpc.getBlockCount()
	if err != nil {
		return err
	}

	// Walk back through settled draws.
	di := blockchain.DrawInterval
	lastDraw := (height - 1) / di * di
	if lastDraw < di {
		fmt.Println("No settled draws yet.")
		return nil
	}

	shown := 0
	for drawH := lastDraw; drawH >= di && shown < n; drawH -= di {
		res, err := rpc.getDrawResult(uint32(drawH))
		if err != nil {
			// Draw with no participants — skip silently.
			continue
		}
		if res.JackpotWon {
			winners := make([]string, len(res.Winners))
			for i, w := range res.Winners {
				winners[i] = fmt.Sprintf("%.8f FDRW → %s", w.PrizeFDRW, w.WalletHash)
			}
			fmt.Printf("Draw #%d  block %d  JACKPOT  pool=%.8f  %s\n",
				res.DrawSequence, drawH, res.TotalPoolFDRW, strings.Join(winners, " | "))
		} else {
			fmt.Printf("Draw #%d  block %d  rollover  pool→%.8f\n",
				res.DrawSequence, drawH, res.RolloverFDRW)
		}
		shown++
	}
	if shown == 0 {
		fmt.Println("No draw results found.")
	}
	return nil
}

// ── Entry point ───────────────────────────────────────────────────────────────

func usage() {
	fmt.Fprintln(os.Stderr, `fdrw — FalconDraw wallet CLI

Usage:
  fdrw [options] <command> [args]

Commands:
  init                       Generate a new wallet key
  info                       Show wallet address and pubkey hash
  balance                    Show spendable and staked balance
  send <address> <amount>    Send FDRW to an address (P2WPKH, ~141 vbytes)
  pool-stake <amount>        Stake <amount> FDRW for the next draw
  check-draw [height]        Show draw state (default: next draw)
  draw-history [n]           Show last N draw results (default: 10)

Options:
  --rpcserver=host:port  Node RPC address (default: localhost:8334)
  --rpcuser=user         RPC username
  --rpcpass=pass         RPC password
  --rpccert=file         TLS certificate file
  --notls                Disable TLS (for local dev nodes)
  --keyfile=path         Wallet key file (default: ~/.fdrw/wallet.key)
  --testnet              Use testnet parameters
  --regtest              Use regtest parameters`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cfg, args, err := loadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	cmd := args[0]
	cmdArgs := args[1:]

	// init and info don't need RPC.
	switch cmd {
	case "init":
		if err := cmdInit(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case "info":
		if err := cmdInfo(cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case "help", "--help", "-h":
		usage()
		return
	}

	rpc, err := newRPCClient(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "RPC client error:", err)
		os.Exit(1)
	}

	switch cmd {
	case "balance":
		err = cmdBalance(cfg, rpc)
	case "send":
		err = cmdSend(cfg, rpc, cmdArgs)
	case "pool-stake":
		err = cmdPoolStake(cfg, rpc, cmdArgs)
	case "check-draw":
		err = cmdCheckDraw(cfg, rpc, cmdArgs)
	case "draw-history":
		err = cmdDrawHistory(cfg, rpc, cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

}
