// Package tspl converts print-ready PNG images into TSPL bitmap jobs.
package tspl

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/png"
	"io"
	"math"
	"strconv"
)

// Options controls label geometry and bitmap conversion.
type Options struct {
	WidthMM     float64
	HeightMM    float64
	DPI         int
	GapMM       float64
	GapOffsetMM float64
	Threshold   uint8
}

// Render fits a PNG into a fixed-size label and writes one TSPL print job.
func Render(w io.Writer, pngData []byte, opt Options) error {
	if opt.WidthMM <= 0 || opt.HeightMM <= 0 {
		return errors.New("tspl: label dimensions must be positive")
	}
	if opt.DPI <= 0 {
		return errors.New("tspl: DPI must be positive")
	}
	if opt.GapMM <= 0 || opt.GapOffsetMM < 0 {
		return errors.New("tspl: invalid gap geometry")
	}
	if opt.Threshold == 0 {
		opt.Threshold = 128
	}

	src, _, err := image.Decode(bytes.NewReader(pngData))
	if err != nil {
		return fmt.Errorf("tspl: decode png: %w", err)
	}
	widthDots := mmToDots(opt.WidthMM, opt.DPI)
	heightDots := mmToDots(opt.HeightMM, opt.DPI)
	if widthDots <= 0 || heightDots <= 0 {
		return errors.New("tspl: label dimensions round to zero dots")
	}
	widthBytes := (widthDots + 7) / 8
	bitmap := fitAndPack(src, widthDots, heightDots, opt.Threshold)

	header := fmt.Sprintf(
		"SIZE %s mm,%s mm\r\nGAP %s mm,%s mm\r\nCLS\r\nBITMAP 0,0,%d,%d,0,",
		number(opt.WidthMM), number(opt.HeightMM), number(opt.GapMM), number(opt.GapOffsetMM),
		widthBytes, heightDots,
	)
	if err := writeAll(w, []byte(header)); err != nil {
		return err
	}
	if err := writeAll(w, bitmap); err != nil {
		return err
	}
	return writeAll(w, []byte("\r\nPRINT 1,1\r\n"))
}

func mmToDots(mm float64, dpi int) int {
	return int(math.Round(mm / 25.4 * float64(dpi)))
}

func number(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// fitAndPack scales src down or up to fit inside the fixed canvas, centers it,
// and packs the printer's inverted bitmap polarity: 1 = white, 0 = black.
func fitAndPack(src image.Image, targetW, targetH int, threshold uint8) []byte {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	widthBytes := (targetW + 7) / 8
	out := make([]byte, widthBytes*targetH)
	for i := range out {
		out[i] = 0xff
	}
	if srcW <= 0 || srcH <= 0 {
		return out
	}

	scale := math.Min(float64(targetW)/float64(srcW), float64(targetH)/float64(srcH))
	contentW := max(1, int(math.Round(float64(srcW)*scale)))
	contentH := max(1, int(math.Round(float64(srcH)*scale)))
	offsetX := (targetW - contentW) / 2
	offsetY := (targetH - contentH) / 2

	rgba := image.NewRGBA(image.Rect(0, 0, srcW, srcH))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	for y := 0; y < contentH; y++ {
		y0 := y * srcH / contentH
		y1 := max(y0+1, (y+1)*srcH/contentH)
		for x := 0; x < contentW; x++ {
			x0 := x * srcW / contentW
			x1 := max(x0+1, (x+1)*srcW/contentW)
			var sum, count int
			for sy := y0; sy < y1 && sy < srcH; sy++ {
				for sx := x0; sx < x1 && sx < srcW; sx++ {
					i := sy*rgba.Stride + sx*4
					lum := (299*int(rgba.Pix[i]) + 587*int(rgba.Pix[i+1]) + 114*int(rgba.Pix[i+2])) / 1000
					if rgba.Pix[i+3] < 128 {
						lum = 255
					}
					sum += lum
					count++
				}
			}
			if count > 0 && sum/count < int(threshold) {
				dx, dy := offsetX+x, offsetY+y
				out[dy*widthBytes+(dx>>3)] &^= 0x80 >> uint(dx&7)
			}
		}
	}
	return out
}
