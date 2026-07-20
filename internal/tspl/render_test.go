package tspl

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestRenderEncodesTSPLBitmapWidthInBytes(t *testing.T) {
	pngData := solidPNG(t, 8, 8, color.Black)
	var out bytes.Buffer

	err := Render(&out, pngData, Options{
		WidthMM:   25.4,
		HeightMM:  25.4,
		DPI:       8,
		GapMM:     2,
		Threshold: 128,
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	prefix := []byte("SIZE 25.4 mm,25.4 mm\r\nGAP 2 mm,0 mm\r\nCLS\r\nBITMAP 0,0,1,8,0,")
	if !bytes.HasPrefix(out.Bytes(), prefix) {
		t.Fatalf("job prefix = %q", out.Bytes())
	}
	payload := out.Bytes()[len(prefix) : len(prefix)+8]
	if !bytes.Equal(payload, bytes.Repeat([]byte{0x00}, 8)) {
		t.Fatalf("bitmap payload = % x, want black rows", payload)
	}
	if got := out.Bytes()[len(prefix)+8:]; !bytes.Equal(got, []byte("\r\nPRINT 1,1\r\n")) {
		t.Fatalf("job suffix = %q", got)
	}
}

func TestRenderFitsImageInsideFixedLabel(t *testing.T) {
	pngData := solidPNG(t, 20, 10, color.Black)
	var out bytes.Buffer

	err := Render(&out, pngData, Options{
		WidthMM:   25.4,
		HeightMM:  25.4,
		DPI:       8,
		GapMM:     2,
		Threshold: 128,
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	marker := []byte("BITMAP 0,0,1,8,0,")
	start := bytes.Index(out.Bytes(), marker)
	if start < 0 {
		t.Fatalf("BITMAP marker missing from %q", out.Bytes())
	}
	payload := out.Bytes()[start+len(marker) : start+len(marker)+8]
	want := []byte{0xff, 0xff, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff}
	if !bytes.Equal(payload, want) {
		t.Fatalf("fitted bitmap = % x, want % x", payload, want)
	}
}

func solidPNG(t *testing.T, width, height int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
