package blockchain

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	wire "github.com/btcsuite/btcd/wire/v2"
)

func TestMakeDrawSettlementMarker(t *testing.T) {
	marker := MakeDrawSettlementMarker(144)
	if len(marker) != DrawSettlementMarkerSize {
		t.Fatalf("marker length %d, want %d", len(marker), DrawSettlementMarkerSize)
	}
	h, ok := extractDrawSettlementMarker(marker)
	if !ok {
		t.Fatal("extractDrawSettlementMarker returned false")
	}
	if h != 144 {
		t.Fatalf("height %d, want 144", h)
	}
}

func TestIsDrawSettlementTx(t *testing.T) {
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxOut(&wire.TxOut{Value: 0, PkScript: MakeDrawSettlementMarker(144)})
	if !IsDrawSettlementTx(btcutil.NewTx(tx)) {
		t.Fatal("expected IsDrawSettlementTx true")
	}

	tx2 := wire.NewMsgTx(wire.TxVersion)
	tx2.AddTxOut(&wire.TxOut{Value: 100, PkScript: []byte{0x76, 0xa9}})
	if IsDrawSettlementTx(btcutil.NewTx(tx2)) {
		t.Fatal("expected IsDrawSettlementTx false for regular tx")
	}
}


