package gzsplice

// CRC-32 (IEEE) combination, zlib's crc32_combine_gen / crc32_combine_op. Appending n bytes to a
// message multiplies its CRC by x^(8n) modulo the CRC polynomial; that operator depends only on n,
// so a stored piece keeps its own (Shift) and joining it to what came before is one 32-step
// carry-less multiply. The standard library has no equivalent: hash/crc32 can only continue a CRC
// over the actual bytes.

// poly is the reflected IEEE polynomial.
const poly = 0xedb88320

// x2n[k] is x^(2^k) modulo poly, in reflected form (1<<31 is x^0).
var x2n = func() (t [32]uint32) {
	p := uint32(1) << 30 // x^1
	t[0] = p
	for n := 1; n < 32; n++ {
		p = multModP(p, p)
		t[n] = p
	}
	return t
}()

// multModP returns a·b modulo poly, both reflected.
func multModP(a, b uint32) uint32 {
	m := uint32(1) << 31
	var p uint32
	for {
		if a&m != 0 {
			p ^= b
			if a&(m-1) == 0 {
				break
			}
		}
		m >>= 1
		if b&1 != 0 {
			b = b>>1 ^ poly
		} else {
			b >>= 1
		}
	}
	return p
}

// Shift returns the operator that appends n bytes: x^(8n) modulo poly.
func Shift(n int64) uint32 {
	p := uint32(1) << 31
	for k := 3; n > 0; n, k = n>>1, k+1 {
		if n&1 != 0 {
			p = multModP(x2n[k&31], p)
		}
	}
	return p
}

// CombineShift returns the CRC of A followed by B from CRC(A), CRC(B) and Shift(len(B)).
func CombineShift(crcA, crcB, shiftB uint32) uint32 { return multModP(shiftB, crcA) ^ crcB }

// Combine returns the CRC of A followed by B from CRC(A), CRC(B) and len(B).
func Combine(crcA, crcB uint32, lenB int64) uint32 { return CombineShift(crcA, crcB, Shift(lenB)) }
