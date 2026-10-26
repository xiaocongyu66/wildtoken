package logbody

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

// randomBody stands in for a request body: random bytes cut the way real text
// does, seeded so a failure reproduces.
func randomBody(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestSplitRebuildsTheInput(t *testing.T) {
	for _, n := range []int{0, 1, minChunk - 1, minChunk, minChunk + 1, MaxChunk, MaxChunk + 1, 1 << 20} {
		in := randomBody(n, uint64(n))
		chunks := Split(in)

		if got := bytes.Join(chunks, nil); !bytes.Equal(got, in) {
			t.Fatalf("%d bytes: rebuilt input differs", n)
		}
		if n == 0 && len(chunks) != 0 {
			t.Fatalf("empty input gave %d chunks", len(chunks))
		}
		for i, c := range chunks {
			last := i == len(chunks)-1
			if len(c) > MaxChunk || len(c) == 0 || (!last && len(c) < minChunk) {
				t.Fatalf("%d bytes: chunk %d is %d bytes", n, i, len(c))
			}
		}
	}
}

// The cut points are part of the stored format: a change to the gear table or
// the cut rule stops new bodies sharing chunks with every body already stored.
// This pins them, so such a change is a decision rather than an accident.
func TestSplitCutsWhereItAlwaysHas(t *testing.T) {
	var offsets []int
	position := 0
	for _, c := range Split(randomBody(64<<10, 42)) {
		position += len(c)
		offsets = append(offsets, position)
	}
	want := []int{26177, 32033, 34952, 51676, 56773, 65536}
	if len(offsets) != len(want) {
		t.Fatalf("cut at %v, want %v", offsets, want)
	}
	for i := range want {
		if offsets[i] != want[i] {
			t.Fatalf("cut at %v, want %v", offsets, want)
		}
	}
}

// The property the package exists for: a conversation that grows by one turn,
// or has a byte changed in the middle, shares nearly every chunk with the one
// before it.
func TestSplitSurvivesLocalEdits(t *testing.T) {
	base := randomBody(1<<20, 11)

	inserted := append(append(bytes.Clone(base[:len(base)/2]), "one new turn"...), base[len(base)/2:]...)
	appended := append(bytes.Clone(base), randomBody(4<<10, 12)...)
	edited := bytes.Clone(base)
	edited[len(edited)/3] ^= 0xff

	known := map[Hash]bool{}
	for _, c := range Split(base) {
		known[Sum(c)] = true
	}

	for name, in := range map[string][]byte{"insert": inserted, "append": appended, "edit": edited} {
		fresh := 0
		for _, c := range Split(in) {
			if !known[Sum(c)] {
				fresh++
			}
		}
		// The changed chunk, plus at most one more where the cut moved.
		if fresh > 3 {
			t.Errorf("%s: %d new chunks, want at most 3", name, fresh)
		}
	}
}

// Storage was sized on this average; drifting far from it would quietly change
// how much a resent conversation costs.
func TestSplitAveragesNearTarget(t *testing.T) {
	in := randomBody(8<<20, 3)
	average := len(in) / len(Split(in))
	if average < avgChunk || average > 2*avgChunk {
		t.Fatalf("average chunk %d bytes, want about %d to %d", average, avgChunk, 2*avgChunk)
	}
}
