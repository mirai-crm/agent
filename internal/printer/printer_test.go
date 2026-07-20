package printer

import (
	"bytes"
	"context"
	"testing"
)

func TestWriteChunkedCompletesShortWrites(t *testing.T) {
	p := &shortWritePrinter{max: 3}
	data := []byte("0123456789")

	if err := WriteChunked(p, data); err != nil {
		t.Fatalf("WriteChunked() error = %v", err)
	}
	if !bytes.Equal(p.data.Bytes(), data) {
		t.Fatalf("written = %q, want %q", p.data.Bytes(), data)
	}
}

type shortWritePrinter struct {
	data bytes.Buffer
	max  int
}

func (*shortWritePrinter) Open(context.Context) error { return nil }
func (*shortWritePrinter) Close() error               { return nil }

func (p *shortWritePrinter) Write(data []byte) (int, error) {
	if len(data) > p.max {
		data = data[:p.max]
	}
	return p.data.Write(data)
}
