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
	"math"
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
		WidthDots: widthDots,
		// Feed enough blank lines past the print head so the whole receipt
		// (incl. the footer) clears the cutter before the partial cut. Each LF
		// ≈ 1/6" (~4.2mm) at the default post-ESC @ line spacing.
		FeedLines:  8,
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

// binarize converts src to a packed 1bpp bitmap fitted to widthDots, preserving
// the source aspect ratio. The server renders super-sampled PNGs (scale>1) at
// widthDots*scale wide AND scale× taller, so we must resample BOTH axes: the
// output width is widthDots and the output height is scaled by the same factor
// (srcH*widthDots/srcW). Each output dot is the box-average of the source pixels
// it covers, then thresholded — anti-aliasing the resize instead of cropping or
// (for the height) leaving it stretched. Equal-width images map 1:1.
func binarize(src image.Image, widthDots int, threshold uint8) *packedMono {
	b := src.Bounds()
	// Normalise to an NRGBA-ish accessor via draw into RGBA for fast pixel reads.
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	srcW := b.Dx()
	srcH := b.Dy()
	widthBytes := (widthDots + 7) / 8
	if srcW <= 0 || srcH <= 0 {
		return &packedMono{widthDots: widthDots, widthBytes: widthBytes}
	}

	// Output height preserves the source aspect ratio at the target width.
	outH := srcH
	if srcW != widthDots {
		outH = int(math.Round(float64(srcH) * float64(widthDots) / float64(srcW)))
		if outH < 1 {
			outH = 1
		}
	}

	m := &packedMono{
		widthDots:  widthDots,
		widthBytes: widthBytes,
		height:     outH,
		data:       make([]byte, widthBytes*outH),
	}
	for y := 0; y < outH; y++ {
		// Source row range covered by output row y.
		y0 := y
		y1 := y + 1
		if srcH != outH {
			y0 = y * srcH / outH
			y1 = (y + 1) * srcH / outH
			if y1 <= y0 {
				y1 = y0 + 1
			}
		}
		rowOff := y * widthBytes
		for x := 0; x < widthDots; x++ {
			// Source column range covered by output dot x.
			x0 := x
			x1 := x + 1
			if srcW != widthDots {
				x0 = x * srcW / widthDots
				x1 = (x + 1) * srcW / widthDots
				if x1 <= x0 {
					x1 = x0 + 1
				}
			}
			var sum, cnt int
			for sy := y0; sy < y1 && sy < srcH; sy++ {
				rowBase := sy * rgba.Stride
				for sx := x0; sx < x1 && sx < srcW; sx++ {
					i := rowBase + sx*4
					lum := int(luminance(rgba.Pix[i], rgba.Pix[i+1], rgba.Pix[i+2]))
					if rgba.Pix[i+3] < 128 { // transparent => white
						lum = 255
					}
					sum += lum
					cnt++
				}
			}
			if cnt == 0 {
				continue // past the source edge: leave white (padding)
			}
			if sum/cnt < int(threshold) {
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
