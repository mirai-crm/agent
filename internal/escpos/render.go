// Package escpos converts print-ready PNG images into an ESC/POS byte stream
// (raster GS v 0) suitable for thermal receipt printers.
package escpos

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/png" // register PNG decoder
	"io"
)

// ESC/POS control bytes.
var (
	cmdInit       = []byte{0x1B, 0x40}       // ESC @
	cmdRasterHdr  = []byte{0x1D, 0x76, 0x30} // GS v 0
	cmdPartialCut = []byte{0x1D, 0x56, 0x01} // GS V 1 (partial cut)
)

// Options controls rendering.
type Options struct {
	// WidthDots is the printer head width in dots (58mm≈384, 80mm≈576).
	// If the decoded image is wider it is cropped; if narrower it is left-padded
	// white so content aligns to the left edge.
	WidthDots int
	// FeedLines is how many blank lines to feed before cutting.
	FeedLines int
	// BandHeight splits the raster into horizontal bands of at most this many
	// rows, each emitted as its own GS v 0 block (improves USB/spooler
	// compatibility). Zero disables banding (single block).
	BandHeight int
	// Threshold is the luminance cutoff [0,255]; pixels darker than this become
	// black dots. mono=1 PNGs are already black/white so the exact value is not
	// critical.
	Threshold uint8
}

// DefaultOptions returns options for the given head width.
func DefaultOptions(widthDots int) Options {
	return Options{
		WidthDots:  widthDots,
		FeedLines:  4,
		BandHeight: 128,
		Threshold:  128,
	}
}

// Render decodes a PNG and writes a full ESC/POS job (init + raster + feed +
// cut) to w. Data may be written in chunks by the caller's Writer.
func Render(w io.Writer, png []byte, opt Options) error {
	if opt.WidthDots <= 0 {
		return errors.New("escpos: WidthDots must be positive")
	}
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		return fmt.Errorf("escpos: decode png: %w", err)
	}

	mono := binarize(img, opt.WidthDots, opt.Threshold)
	widthBytes := (opt.WidthDots + 7) / 8
	height := mono.Bounds().Dy()

	if _, err := w.Write(cmdInit); err != nil {
		return err
	}

	band := opt.BandHeight
	if band <= 0 || band > height {
		band = height
	}
	for y0 := 0; y0 < height; y0 += band {
		rows := band
		if y0+rows > height {
			rows = height - y0
		}
		if err := writeRasterBlock(w, mono, y0, rows, widthBytes, opt.WidthDots); err != nil {
			return err
		}
	}

	for i := 0; i < opt.FeedLines; i++ {
		if _, err := w.Write([]byte{0x0A}); err != nil {
			return err
		}
	}
	if _, err := w.Write(cmdPartialCut); err != nil {
		return err
	}
	return nil
}

// writeRasterBlock emits one GS v 0 block for rows [y0, y0+rows).
func writeRasterBlock(w io.Writer, mono *packedMono, y0, rows, widthBytes, widthDots int) error {
	if rows <= 0 {
		return nil
	}
	if rows > 0xFFFF {
		return fmt.Errorf("escpos: band height %d exceeds GS v 0 limit", rows)
	}
	header := make([]byte, 0, len(cmdRasterHdr)+5)
	header = append(header, cmdRasterHdr...)
	header = append(header, 0x00) // m = normal
	header = append(header, byte(widthBytes&0xFF), byte((widthBytes>>8)&0xFF))
	header = append(header, byte(rows&0xFF), byte((rows>>8)&0xFF))
	if _, err := w.Write(header); err != nil {
		return err
	}
	// Row data: widthBytes per row, MSB first, bit=1 => black.
	for y := y0; y < y0+rows; y++ {
		if _, err := w.Write(mono.row(y)); err != nil {
			return err
		}
	}
	return nil
}

// packedMono is a 1bpp bitmap, MSB-first, bit=1 => black dot.
type packedMono struct {
	widthDots  int
	widthBytes int
	height     int
	data       []byte // height * widthBytes
}

func (m *packedMono) Bounds() image.Rectangle {
	return image.Rect(0, 0, m.widthDots, m.height)
}

func (m *packedMono) row(y int) []byte {
	return m.data[y*m.widthBytes : (y+1)*m.widthBytes]
}

// binarize converts src to a packed 1bpp bitmap fitted to widthDots. Wider
// images are cropped to widthDots; narrower ones are left-aligned with white
// padding on the right.
func binarize(src image.Image, widthDots int, threshold uint8) *packedMono {
	b := src.Bounds()
	// Normalise to an NRGBA-ish accessor via draw into RGBA for fast pixel reads.
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	srcW := b.Dx()
	height := b.Dy()
	widthBytes := (widthDots + 7) / 8
	m := &packedMono{
		widthDots:  widthDots,
		widthBytes: widthBytes,
		height:     height,
		data:       make([]byte, widthBytes*height),
	}
	for y := 0; y < height; y++ {
		rowOff := y * widthBytes
		for x := 0; x < widthDots && x < srcW; x++ {
			i := rgba.PixOffset(x, y)
			r := rgba.Pix[i]
			g := rgba.Pix[i+1]
			bl := rgba.Pix[i+2]
			a := rgba.Pix[i+3]
			// Treat fully/again transparent as white.
			lum := luminance(r, g, bl)
			if a < 128 {
				lum = 255
			}
			if lum < threshold {
				m.data[rowOff+(x>>3)] |= 0x80 >> uint(x&7)
			}
		}
	}
	return m
}

func luminance(r, g, b uint8) uint8 {
	// Rec. 601 luma; integer math.
	return uint8((299*int(r) + 587*int(g) + 114*int(b)) / 1000)
}
