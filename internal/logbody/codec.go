package logbody

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"sync"
)

// Codec says how stored bytes are encoded. The values are stored in SQLite,
// so they never change meaning.
type Codec int64

const (
	CodecRaw   Codec = 0
	CodecFlate Codec = 1
)

// ErrCorrupt reports stored bytes that do not decode to what was stored.
var ErrCorrupt = errors.New("logbody: corrupt stored data")

// flate writers allocate about a megabyte each, so they are reused rather than
// built per chunk.
var writers = sync.Pool{New: func() any {
	w, _ := flate.NewWriter(io.Discard, flate.DefaultCompression)
	return w
}}

var readers = sync.Pool{New: func() any {
	return flate.NewReader(bytes.NewReader(nil))
}}

// Pack compresses b for storage. Bytes that do not shrink — an image, an
// archive — are kept raw instead of paying to inflate them on every read.
func Pack(b []byte) (Codec, []byte) {
	var buf bytes.Buffer
	buf.Grow(len(b)/2 + 64)

	w := writers.Get().(*flate.Writer)
	defer writers.Put(w)
	w.Reset(&buf)
	// Writing to a bytes.Buffer cannot fail.
	_, _ = w.Write(b)
	_ = w.Close()

	if buf.Len() >= len(b) {
		return CodecRaw, b
	}
	return CodecFlate, buf.Bytes()
}

// Unpack reverses Pack. size is the length Pack was given; stored bytes that
// decode to anything else are damage, not data. Raw DEFLATE carries no
// checksum, so a caller that knows the content hash should verify it too.
func Unpack(codec Codec, data []byte, size int) ([]byte, error) {
	switch codec {
	case CodecRaw:
		if len(data) != size {
			return nil, ErrCorrupt
		}
		return data, nil
	case CodecFlate:
	default:
		return nil, ErrCorrupt
	}

	r := readers.Get().(io.ReadCloser)
	defer readers.Put(r)
	if err := r.(flate.Resetter).Reset(bytes.NewReader(data), nil); err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}

	// One byte past size tells a stream that is too long from one that fits.
	out := make([]byte, 0, size)
	buf := bytes.NewBuffer(out)
	if _, err := buf.ReadFrom(io.LimitReader(r, int64(size)+1)); err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	if buf.Len() != size {
		return nil, ErrCorrupt
	}
	return buf.Bytes(), nil
}
