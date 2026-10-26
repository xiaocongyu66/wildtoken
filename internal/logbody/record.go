package logbody

import (
	"encoding/binary"
	"errors"
)

// The four snapshots a log row records, in record order.
const (
	SlotDownstreamRequest = iota
	SlotUpstreamRequest
	SlotUpstreamResponse
	SlotDownstreamResponse
	SlotCount
)

const recordVersion = 1

// Record flags, one byte per slot that has captured bytes.
const (
	flagTruncated = 1 << iota
	// flagSameChunks reuses the previous slot's chunk list: an upstream request
	// nothing rewrote, or the one response both sides saw.
	flagSameChunks
)

// ErrBadRecord reports a body record that does not decode.
var ErrBadRecord = errors.New("logbody: malformed body record")

// SlotBody is one slot's body as stored: how it renders and which chunks hold
// its captured bytes.
type SlotBody struct {
	Kind      BodyKind
	Length    int64
	Truncated bool
	// Captured is the number of bytes across Chunks.
	Captured int64
	// Chunks are chunk ids in body order.
	Chunks []int64
}

// Record is the stored form of a log row's bodies.
type Record [SlotCount]SlotBody

// Empty reports a record with no slot worth storing.
func (r *Record) Empty() bool {
	for i := range r {
		if r[i].Kind != KindNone {
			return false
		}
	}
	return true
}

// AppendRecord encodes r. Chunk ids go in as varint deltas: ids are assigned
// in insertion order, so a body's new chunks sit next to each other and most
// deltas take one byte. 200 chunks cost about 250 bytes, against 6.4 KB as
// SHA-256 hashes.
//
//	uvarint version
//	per slot: byte kind
//	  omitted:      uvarint length
//	  text, binary: uvarint length, byte flags,
//	                unless same chunks: uvarint captured, uvarint n, n varint deltas
func AppendRecord(dst []byte, r *Record) []byte {
	dst = binary.AppendUvarint(dst, recordVersion)
	for i := range r {
		slot := &r[i]
		dst = append(dst, byte(slot.Kind))
		switch slot.Kind {
		case KindOmitted:
			dst = binary.AppendUvarint(dst, uint64(slot.Length))
		case KindText, KindBinary:
			dst = binary.AppendUvarint(dst, uint64(slot.Length))
			var flags byte
			if slot.Truncated {
				flags |= flagTruncated
			}
			same := i > 0 && sameChunks(&r[i-1], slot)
			if same {
				flags |= flagSameChunks
			}
			dst = append(dst, flags)
			if same {
				continue
			}
			dst = binary.AppendUvarint(dst, uint64(slot.Captured))
			dst = binary.AppendUvarint(dst, uint64(len(slot.Chunks)))
			var previous int64
			for _, id := range slot.Chunks {
				dst = binary.AppendVarint(dst, id-previous)
				previous = id
			}
		}
	}
	return dst
}

// sameChunks reports a slot whose bytes the previous slot already lists.
func sameChunks(previous, slot *SlotBody) bool {
	if (previous.Kind != KindText && previous.Kind != KindBinary) ||
		previous.Captured != slot.Captured || len(previous.Chunks) != len(slot.Chunks) ||
		len(slot.Chunks) == 0 {
		return false
	}
	for i := range slot.Chunks {
		if previous.Chunks[i] != slot.Chunks[i] {
			return false
		}
	}
	return true
}

// DecodeRecord reverses AppendRecord. It accepts nothing it would not have
// written: a record that half-decodes would point a body at the wrong chunks.
func DecodeRecord(b []byte) (Record, error) {
	var r Record
	d := decoder{b: b}
	if d.uvarint() != recordVersion {
		return r, ErrBadRecord
	}
	for i := range r {
		slot := &r[i]
		slot.Kind = BodyKind(d.byte())
		switch slot.Kind {
		case KindNone:
		case KindOmitted:
			slot.Length = d.length()
		case KindText, KindBinary:
			slot.Length = d.length()
			flags := d.byte()
			if flags&^(flagTruncated|flagSameChunks) != 0 {
				d.fail()
			}
			slot.Truncated = flags&flagTruncated != 0
			if flags&flagSameChunks != 0 {
				previous := r[max(i-1, 0)]
				if i == 0 || (previous.Kind != KindText && previous.Kind != KindBinary) ||
					len(previous.Chunks) == 0 {
					d.fail()
					break
				}
				slot.Captured, slot.Chunks = previous.Captured, previous.Chunks
				continue
			}
			slot.Captured = d.length()
			n := d.uvarint()
			// Every chunk holds at least one byte and at most MaxChunk.
			if d.err != nil || n > uint64(slot.Captured) ||
				uint64(slot.Captured) > n*MaxChunk || slot.Captured > slot.Length {
				d.fail()
				break
			}
			slot.Chunks = make([]int64, n)
			var previous int64
			for j := range slot.Chunks {
				previous += d.varint()
				if previous <= 0 {
					d.fail()
					break
				}
				slot.Chunks[j] = previous
			}
		default:
			d.fail()
		}
		if d.err != nil {
			return Record{}, d.err
		}
	}
	if d.err == nil && len(d.b) != 0 {
		d.fail()
	}
	if d.err != nil {
		return Record{}, d.err
	}
	return r, nil
}

// decoder reads a record, remembering the first failure so the caller checks
// once.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) fail() { d.err = ErrBadRecord }

func (d *decoder) byte() byte {
	if d.err != nil || len(d.b) == 0 {
		d.fail()
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.b = d.b[n:]
	return v
}

// length reads a byte count, which must fit an int64.
func (d *decoder) length() int64 {
	v := d.uvarint()
	if v > 1<<62 {
		d.fail()
		return 0
	}
	return int64(v)
}
