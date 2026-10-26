// Package logbody stores captured request and response bodies by content.
//
// A coding agent resends its whole conversation on every turn, so consecutive
// request bodies share nearly all their bytes. A body is cut where its content
// says rather than at fixed offsets: a turn appended at the end leaves every
// earlier cut in place, so only the chunks it touches are new, and each chunk is
// stored once however many bodies contain it.
package logbody

import "crypto/sha256"

// Chunk sizes, in bytes. No cut falls inside the first minChunk bytes of a
// chunk, one always falls at MaxChunk, and in between one lands on average
// every avgChunk bytes of input.
const (
	minChunk = 2 << 10
	avgChunk = 8 << 10
	MaxChunk = 64 << 10
)

// cutShift turns the 64-bit gear hash into a 1-in-avgChunk event: a cut is made
// when the hash's top 13 bits are all zero.
const cutShift = 64 - 13

// gear holds one pseudo-random value per byte value, from splitmix64 with a
// fixed seed. It is generated here rather than taken from a library so a body
// cuts the same way on every build. A changed table would stop new bodies
// sharing chunks with stored ones; it would cost space, never correctness,
// because a body is rebuilt from its own list of chunks however it was cut.
var gear = func() (table [256]uint64) {
	x := uint64(0x77696c64746f6b6e) // "wildtokn"
	for i := range table {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
		z = (z ^ z>>27) * 0x94d049bb133111eb
		table[i] = z ^ z>>31
	}
	return table
}()

// Hash names a chunk or a metadata blob by its content.
type Hash = [sha256.Size]byte

// Sum hashes stored bytes.
func Sum(b []byte) Hash { return sha256.Sum256(b) }

// Split cuts b into content-defined chunks. The chunks alias b and, joined in
// order, are b again; an empty b yields none.
func Split(b []byte) [][]byte {
	var chunks [][]byte
	for len(b) > 0 {
		n := cut(b)
		chunks = append(chunks, b[:n])
		b = b[n:]
	}
	return chunks
}

// cut returns the length of the next chunk. The gear hash at a position depends
// only on the 64 bytes before it, so a cut moves with the content around it,
// not with its offset in the body.
func cut(b []byte) int {
	n := len(b)
	if n <= minChunk {
		return n
	}
	n = min(n, MaxChunk)
	var h uint64
	for i := minChunk; i < n; i++ {
		h = (h << 1) + gear[b[i]]
		if h>>cutShift == 0 {
			return i + 1
		}
	}
	return n
}
