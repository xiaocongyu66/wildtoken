package logbody

import "bytes"

// Chunk is a piece of a body on its way to storage.
type Chunk struct {
	Hash Hash
	// Data aliases the body it was cut from.
	Data []byte
	// Codec and Packed hold the stored form. They are filled in only for a
	// chunk that turned out to be new: most of a resent conversation is stored
	// already and never needs compressing.
	Codec  Codec
	Packed []byte
}

// Pack compresses the chunk unless that was already done.
func (c *Chunk) Pack() {
	if c.Packed == nil {
		c.Codec, c.Packed = Pack(c.Data)
	}
}

// PreparedSlot is one snapshot, hashed and cut.
type PreparedSlot struct {
	// Snapshot is nil when the row has no such snapshot.
	Snapshot *Snapshot
	MetaHash Hash
	// Chunks hold the captured body bytes, in order.
	Chunks []*Chunk
}

// Prepared is a log row's snapshots, ready to store.
type Prepared [SlotCount]PreparedSlot

// Batch cuts the snapshots of one write.
//
// Rows in a batch often share chunks — consecutive turns of one conversation, a
// request and its retry — so the batch keeps one *Chunk per distinct hash, and
// each is compressed at most once.
type Batch struct {
	chunks map[Hash]*Chunk
	order  []*Chunk
}

func NewBatch() *Batch {
	return &Batch{chunks: map[Hash]*Chunk{}}
}

// Chunks lists the batch's distinct chunks in the order they were first cut.
func (b *Batch) Chunks() []*Chunk { return b.order }

// Prepare hashes and cuts one row's snapshots.
//
// The request pair and the response pair are usually the same snapshot, or
// the same bytes: an upstream request nothing rewrote, the one response both
// sides saw. Such a slot reuses its neighbour's hash and chunks instead of
// hashing the body a second time.
func (b *Batch) Prepare(snapshots [SlotCount]*Snapshot) *Prepared {
	var p Prepared
	for i, s := range snapshots {
		if s == nil {
			continue
		}
		slot := &p[i]
		slot.Snapshot = s

		var previous *Snapshot
		if i > 0 {
			previous = snapshots[i-1]
		}
		if previous != nil && (previous == s || bytes.Equal(previous.Meta, s.Meta)) {
			slot.MetaHash = p[i-1].MetaHash
		} else {
			slot.MetaHash = Sum(s.Meta)
		}

		if !hasBytes(s.Body) {
			continue
		}
		if previous != nil && hasBytes(previous.Body) && sameBytes(previous.Body.Data, s.Body.Data) {
			slot.Chunks = p[i-1].Chunks
			continue
		}
		for _, piece := range Split(s.Body.Data) {
			h := Sum(piece)
			c := b.chunks[h]
			if c == nil {
				c = &Chunk{Hash: h, Data: piece}
				b.chunks[h] = c
				b.order = append(b.order, c)
			}
			slot.Chunks = append(slot.Chunks, c)
		}
	}
	return &p
}

// Record lists the row's bodies with the chunk ids storage assigned.
func (p *Prepared) Record(idOf func(*Chunk) int64) Record {
	var r Record
	for i := range p {
		s := p[i].Snapshot
		if s == nil || s.Body == nil {
			continue
		}
		body, slot := s.Body, &r[i]
		slot.Kind, slot.Length = body.Kind, body.Length
		if body.Kind != KindText && body.Kind != KindBinary {
			continue
		}
		slot.Truncated = body.Truncated
		slot.Captured = int64(len(body.Data))
		slot.Chunks = make([]int64, len(p[i].Chunks))
		for j, c := range p[i].Chunks {
			slot.Chunks[j] = idOf(c)
		}
	}
	return r
}

// hasBytes reports a body with captured bytes to store.
func hasBytes(body *Body) bool {
	return body != nil && (body.Kind == KindText || body.Kind == KindBinary) && len(body.Data) > 0
}

// sameBytes compares two bodies, cheaply when they share a buffer.
func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 || &a[0] == &b[0] {
		return true
	}
	return bytes.Equal(a, b)
}
