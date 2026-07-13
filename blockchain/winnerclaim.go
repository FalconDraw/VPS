package blockchain

// MakeWinnerP2WPKHScript returns the 22-byte P2WPKH output script for the
// winning wallet. The settlement transaction mints the prize pool directly
// into this standard spendable output — no claim transaction required.
//
// Script layout: OP_0 OP_DATA_20 <winnerHash[20]>
func MakeWinnerP2WPKHScript(winnerHash [20]byte) []byte {
	script := make([]byte, 22)
	script[0] = 0x00 // OP_0
	script[1] = 0x14 // OP_DATA_20
	copy(script[2:], winnerHash[:])
	return script
}
