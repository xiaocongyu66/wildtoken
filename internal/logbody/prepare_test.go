package logbody

import (
	"bytes"
	"testing"
)

func textSnapshot(meta string, body []byte) *Snapshot {
	return &Snapshot{Meta: []byte(meta), Body: CaptureBody(body, len(body), len(body))}
}

// A request nothing rewrote and the one response both sides saw are the same
// snapshot twice; neither is hashed or listed a second time.
func TestPrepareSharesAPairThatIsTheSame(t *testing.T) {
	conversation := randomBody(256<<10, 1)
	request := textSnapshot(`{"method":"POST"}`, conversation)
	response := textSnapshot(`{"status":200}`, []byte("ok"))

	batch := NewBatch()
	prepared := batch.Prepare([SlotCount]*Snapshot{request, request, response, response})

	if prepared[SlotUpstreamRequest].MetaHash != prepared[SlotDownstreamRequest].MetaHash ||
		&prepared[SlotUpstreamRequest].Chunks[0] != &prepared[SlotDownstreamRequest].Chunks[0] {
		t.Error("the upstream request was cut again")
	}

	// A copy with the same bytes shares the list too.
	copied := textSnapshot(`{"method":"POST"}`, bytes.Clone(conversation))
	prepared = batch.Prepare([SlotCount]*Snapshot{request, copied, nil, nil})
	if len(prepared[SlotUpstreamRequest].Chunks) != len(prepared[SlotDownstreamRequest].Chunks) {
		t.Error("an identical copy was not recognised")
	}

	ids := map[*Chunk]int64{}
	for i, c := range batch.Chunks() {
		ids[c] = int64(i + 1)
	}
	record := prepared.Record(func(c *Chunk) int64 { return ids[c] })
	encoded := AppendRecord(nil, &record)
	decoded, err := DecodeRecord(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded[SlotUpstreamRequest].Captured != int64(len(conversation)) || decoded[SlotUpstreamResponse].Kind != KindNone {
		t.Errorf("record = %+v", decoded)
	}
}

// Consecutive turns of one conversation in a batch share every chunk but the
// tail, and each shared chunk is one *Chunk, compressed once.
func TestPrepareSharesChunksAcrossRows(t *testing.T) {
	turn := randomBody(512<<10, 2)
	next := append(bytes.Clone(turn), randomBody(4<<10, 3)...)

	batch := NewBatch()
	first := batch.Prepare([SlotCount]*Snapshot{textSnapshot(`{}`, turn)})
	before := len(batch.Chunks())
	second := batch.Prepare([SlotCount]*Snapshot{textSnapshot(`{}`, next)})

	if added := len(batch.Chunks()) - before; added > 3 {
		t.Errorf("the next turn added %d chunks, want at most 3", added)
	}
	if first[0].Chunks[0] != second[0].Chunks[0] {
		t.Error("a shared chunk was not the same *Chunk")
	}
}

func TestOmittedAndEmptyBodiesNeedNoChunks(t *testing.T) {
	omitted := &Snapshot{Meta: []byte(`{}`), Body: CaptureBody([]byte("secret"), 0, 6)}
	empty := &Snapshot{Meta: []byte(`{}`), Body: CaptureBody([]byte{}, 10, 0)}
	prepared := NewBatch().Prepare([SlotCount]*Snapshot{omitted, empty})
	if len(prepared[0].Chunks) != 0 || len(prepared[1].Chunks) != 0 {
		t.Fatal("a body without captured bytes was cut into chunks")
	}

	record := prepared.Record(func(*Chunk) int64 { t.Fatal("no chunk should need an id"); return 0 })
	if record[0].Kind != KindOmitted || record[0].Length != 6 || record[1].Kind != KindText || record[1].Captured != 0 {
		t.Errorf("record = %+v", record)
	}
}
