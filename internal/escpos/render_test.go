package escpos

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// makePNG builds a w x h PNG that is all black.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: 0}) // black
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestRenderEmitsInitRasterAndCut(t *testing.T) {
	data := makePNG(t, 8, 3)
	var out bytes.Buffer
	opt := Options{WidthDots: 8, FeedLines: 2, BandHeight: 0, Threshold: 128}
	if err := Render(&out, data, opt); err != nil {
		t.Fatalf("render: %v", err)
	}
	b := out.Bytes()

	// Must start with ESC @.
	if !bytes.HasPrefix(b, []byte{0x1B, 0x40}) {
		t.Fatalf("missing ESC @ init, got % x", b[:2])
	}
	// GS v 0 header for width 8 (1 byte/row), height 3.
	want := []byte{0x1D, 0x76, 0x30, 0x00, 0x01, 0x00, 0x03, 0x00}
	if !bytes.Contains(b, want) {
		t.Fatalf("missing GS v 0 header % x in % x", want, b)
	}
	// All-black 8px row => 0xFF, three rows.
	if !bytes.Contains(b, []byte{0xFF, 0xFF, 0xFF}) {
		t.Fatalf("expected three 0xFF raster rows in % x", b)
	}
	// Must end with partial cut GS V 1.
	if !bytes.HasSuffix(b, []byte{0x1D, 0x56, 0x01}) {
		t.Fatalf("missing partial cut suffix, got % x", b[len(b)-3:])
	}
}

func TestRenderBandingSplitsBlocks(t *testing.T) {
	data := makePNG(t, 8, 10)
	var out bytes.Buffer
	opt := Options{WidthDots: 8, FeedLines: 0, BandHeight: 4, Threshold: 128}
	if err := Render(&out, data, opt); err != nil {
		t.Fatalf("render: %v", err)
	}
	// 10 rows / band 4 => 3 raster blocks (4+4+2).
	n := bytes.Count(out.Bytes(), []byte{0x1D, 0x76, 0x30})
	if n != 3 {
		t.Fatalf("expected 3 GS v 0 blocks, got %d", n)
	}
}
