package logbody

import (
	"errors"
	"reflect"
	"testing"
)

func TestRecordRoundTrips(t *testing.T) {
	record := Record{
		SlotDownstreamRequest: {Kind: KindText, Length: 30000, Captured: 20000, Truncated: true,
			Chunks: []int64{7, 8, 9, 3, 1 << 40}},
		SlotUpstreamRequest: {Kind: KindText, Length: 30000, Captured: 20000, Truncated: true,
			Chunks: []int64{7, 8, 9, 3, 1 << 40}},
		SlotUpstreamResponse: {Kind: KindOmitted, Length: 512},
		// SlotDownstreamResponse is absent.
	}
	encoded := AppendRecord(nil, &record)
	decoded, err := DecodeRecord(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, record) {
		t.Fatalf("decoded %+v, want %+v", decoded, record)
	}
}

// Consecutive chunk ids cost a byte each, and a slot repeating its neighbour's
// list costs two bytes in all.
func TestRecordsStayCompact(t *testing.T) {
	ids := make([]int64, 200)
	for i := range ids {
		ids[i] = 1_000_000 + int64(i)
	}
	record := Record{
		SlotDownstreamRequest: {Kind: KindText, Length: 1 << 20, Captured: 1 << 20, Chunks: ids},
		SlotUpstreamRequest:   {Kind: KindText, Length: 1 << 20, Captured: 1 << 20, Chunks: ids},
	}
	if n := len(AppendRecord(nil, &record)); n > 230 {
		t.Fatalf("record is %d bytes, want about one per chunk", n)
	}
}

func TestDecodeRecordRefusesDamage(t *testing.T) {
	valid := AppendRecord(nil, &Record{
		SlotDownstreamRequest: {Kind: KindBinary, Length: 10, Captured: 10, Chunks: []int64{5}},
	})
	if _, err := DecodeRecord(valid); err != nil {
		t.Fatalf("the valid record failed: %v", err)
	}

	for name, damaged := range map[string][]byte{
		"empty":            {},
		"version":          append([]byte{2}, valid[1:]...),
		"cut short":        valid[:len(valid)-1],
		"trailing bytes":   append(append([]byte{}, valid...), 0),
		"unknown kind":     {1, 9, 0, 0, 0},
		"unknown flag":     {1, byte(KindText), 10, 0x80, 0, 0, 0, 0},
		"leading repeat":   {1, byte(KindText), 10, flagSameChunks, 0, 0, 0},
		"zero chunk id":    {1, byte(KindText), 10, 0, 10, 1, 0, 0, 0, 0},
		"more chunks":      {1, byte(KindText), 10, 0, 2, 3, 2, 2, 2, 0, 0, 0},
		"captured too big": {1, byte(KindText), 2, 0, 10, 1, 2, 0, 0, 0},
	} {
		if _, err := DecodeRecord(damaged); !errors.Is(err, ErrBadRecord) {
			t.Errorf("%s: err = %v, want ErrBadRecord", name, err)
		}
	}
}
