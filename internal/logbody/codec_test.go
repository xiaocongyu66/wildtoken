package logbody

import (
	"bytes"
	"errors"
	"testing"
)

func TestPackRoundTrips(t *testing.T) {
	text := bytes.Repeat([]byte(`{"role":"user","content":"hello"}`), 2000)
	for name, in := range map[string][]byte{
		"empty":  {},
		"text":   text,
		"random": randomBody(MaxChunk, 5),
	} {
		codec, packed := Pack(in)
		out, err := Unpack(codec, packed, len(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(out, in) {
			t.Fatalf("%s: round trip differs", name)
		}
	}
}

// Random bytes do not shrink, so they are stored as they are rather than paid
// for again on every read.
func TestIncompressibleBytesAreStoredRaw(t *testing.T) {
	in := randomBody(MaxChunk, 6)
	if codec, packed := Pack(in); codec != CodecRaw || !bytes.Equal(packed, in) {
		t.Fatalf("codec = %d, want raw bytes", codec)
	}
	if codec, _ := Pack(bytes.Repeat([]byte("a"), 4096)); codec != CodecFlate {
		t.Fatalf("codec = %d, want repetitive text compressed", codec)
	}
}

func TestUnpackRefusesDamage(t *testing.T) {
	codec, packed := Pack(bytes.Repeat([]byte("abc"), 1000))

	for name, attempt := range map[string]func() error{
		"garbage":       func() error { _, err := Unpack(CodecFlate, []byte("not deflate"), 3000); return err },
		"longer":        func() error { _, err := Unpack(codec, packed, 2999); return err },
		"shorter":       func() error { _, err := Unpack(codec, packed, 3001); return err },
		"raw length":    func() error { _, err := Unpack(CodecRaw, []byte("abc"), 4); return err },
		"unknown codec": func() error { _, err := Unpack(Codec(9), packed, 3000); return err },
	} {
		if err := attempt(); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
}
