package compress

import (
	"bytes"
	"compress/zlib"
	"io"

	"github.com/pkg/errors"
)

// Decompressor is a fast, single threaded compressor.
// This type allows us to reuse buffers etc for performance
type Decompressor interface {
	// Decompress decompresses the byte array
	Decompress(b []byte) ([]byte, error)
}

// NoOpDecompressor is a DeCompressor that does nothing.  Useful for tests.
type NoOpDecompressor struct {
}

func (c *NoOpDecompressor) DeCompress(b []byte) ([]byte, error) {
	return b, nil
}

// ZlibDecompressor decompresses Zlib,
type ZlibDecompressor struct {
	outputBuffer *bytes.Buffer
	inputBuffer  *bytes.Buffer
	reader       io.ReadCloser
}

func NewZlibDecompressor() (*ZlibDecompressor, error) {
	var ib bytes.Buffer
	var ob bytes.Buffer
	return &ZlibDecompressor{
		inputBuffer:  &ib,
		outputBuffer: &ob,
	}, nil
}

func (d *ZlibDecompressor) Decompress(b []byte) ([]byte, error) {
	inputBuffer := bytes.NewBuffer(b)
	if d.reader == nil {
		reader, err := zlib.NewReader(inputBuffer)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		d.reader = reader
	} else {
		err := d.reader.(zlib.Resetter).Reset(inputBuffer, nil)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		d.outputBuffer.Reset()
	}

	// Decompress
	_, err := io.Copy(d.outputBuffer, d.reader)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	decompressed := d.outputBuffer.Bytes()
	return decompressed, nil
}
