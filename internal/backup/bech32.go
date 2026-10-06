package backup

import "strings"

// bech32 encodes key material the way age does (BIP-173 bech32, without the
// 90-character limit), so a key derived from the words can be handed to
// age.ParseHybridIdentity and to the stock age tool. age keeps its own
// encoder internal.

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// convert8to5 regroups bytes into 5-bit groups, padding the last one.
func convert8to5(data []byte) []byte {
	var out []byte
	acc, bits := uint32(0), uint(0)
	for _, b := range data {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, byte(acc>>bits)&31)
		}
	}
	if bits > 0 {
		out = append(out, byte(acc<<(5-bits))&31)
	}
	return out
}

// bech32Encode returns hrp + "1" + data + checksum, in lower case.
func bech32Encode(hrp string, data []byte) string {
	hrp = strings.ToLower(hrp)
	values := convert8to5(data)
	check := append(bech32HRPExpand(hrp), values...)
	check = append(check, 0, 0, 0, 0, 0, 0)
	mod := bech32Polymod(check) ^ 1
	var b strings.Builder
	b.Grow(len(hrp) + 1 + len(values) + 6)
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, v := range values {
		b.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(bech32Charset[(mod>>uint(5*(5-i)))&31])
	}
	return b.String()
}
