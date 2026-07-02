// Command testprint is a standalone diagnostic for the ESC/POS print pipeline.
//
// It fetches a check/z-report PNG from the CRM exactly like the worker does
// (paper width and scale derived from the device config), renders the ESC/POS
// raster, reports the decoded dimensions vs the configured head width, and can
// optionally send the job to the CUPS raw queue.
//
// It can also print a calibration grid (--calib) to measure a printer's
// horizontal/vertical aspect ratio.
//
// Usage examples:
//
//	# Dry run: inspect what the server returns for check 5 (uses config):
//	go run ./scripts/testprint --config config.toml --check 5
//
//	# Print check 5:
//	go run ./scripts/testprint --config config.toml --check 5 --print
//
//	# Calibration grid (576x576 dots, 10mm cells) to check aspect ratio:
//	go run ./scripts/testprint --config config.toml --calib 576 --print
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/config"
	"github.com/mirai-agent/mirai-agent/internal/escpos"
)

func main() {
	var (
		cfgPath  = flag.String("config", "config.toml", "path to config.toml")
		deviceID = flag.Int64("device-id", 0, "device id to use (0 = first device)")
		checkID  = flag.Int64("check", 0, "checkId to print via /checks/{id}/png")
		zReport  = flag.Int64("zreport", 0, "zReportId to print via /z-reports/{id}/png")
		widthMM  = flag.Int("width", -1, "paper width in mm (58|80) sent as ?width; -1 = derive from device width_dots (mirrors worker)")
		scale    = flag.Int("scale", -1, "?scale (>=1); -1 = use device png_scale from config; 0 = omit scale")
		band     = flag.Int("band", -1, "GS v 0 band height in rows; -1 = default (128), 0 = single block (no banding)")
		calib    = flag.Int("calib", 0, "calibration mode: print an NxN-dot grid (cells every 80 dots = 10mm) instead of a document; 0 = off")
		doPrint  = flag.Bool("print", false, "actually send the job to the CUPS raw queue")
		outPNG   = flag.String("out-png", "testprint.png", "where to save the fetched PNG")
		outRaw   = flag.String("out-raw", "testprint.escpos.bin", "where to save the raw ESC/POS bytes")
	)
	flag.Parse()

	if *calib <= 0 && *checkID <= 0 && *zReport <= 0 {
		fmt.Fprintln(os.Stderr, "provide --check <id>, --zreport <id>, or --calib <dots>")
		os.Exit(2)
	}

	cfg, _, err := config.LoadRaw(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if len(cfg.Devices) == 0 {
		fmt.Fprintln(os.Stderr, "no [[devices]] in config")
		os.Exit(1)
	}

	dev := cfg.Devices[0]
	if *deviceID != 0 {
		found := false
		for _, d := range cfg.Devices {
			if d.ID == *deviceID {
				dev = d
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "device id %d not found in config\n", *deviceID)
			os.Exit(1)
		}
	}

	effScale := *scale
	if effScale < 0 {
		// Mirror the worker: use the device's configured png_scale.
		effScale = dev.PNGScale
	}

	effWidth := *widthMM
	if effWidth < 0 {
		// Mirror the worker: derive paper width from the head width in dots.
		effWidth = dev.PaperWidthMM()
	}

	var path string
	if *checkID > 0 {
		path = fmt.Sprintf("/api/v1/devices/checks/%d/png", *checkID)
	} else {
		path = fmt.Sprintf("/api/v1/devices/z-reports/%d/png", *zReport)
	}

	fmt.Printf("device: id=%d name=%q width_dots=%d png_scale=%d printer=%s/%s\n",
		dev.ID, dev.Name, dev.WidthDots, dev.PNGScale, dev.Printer.Kind, dev.Printer.Queue)

	var pngBytes []byte
	if *calib > 0 {
		pngBytes = makeCalibPNG(*calib)
		fmt.Printf("CALIBRATION: %dx%d dots grid, cells every 80 dots (=10mm @ 203dpi)\n", *calib, *calib)
	} else {
		fmt.Printf("request: %s  width=%d scale=%d\n", path, effWidth, effScale)
		client := api.New(api.Config{
			BaseURL:         cfg.Server.BaseURL,
			Token:           dev.Token,
			RequestTimeout:  cfg.HTTP.RequestTimeout(),
			LongpollTimeout: cfg.HTTP.LongpollTimeout(),
		})
		ctx := context.Background()
		b, err := client.FetchPNG(ctx, path, effWidth, effScale)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fetch png: %v\n", err)
			os.Exit(1)
		}
		pngBytes = b
	}
	png := pngBytes

	imgCfg, _, decErr := image.DecodeConfig(bytes.NewReader(png))
	if decErr != nil {
		fmt.Fprintf(os.Stderr, "decode png config: %v\n", decErr)
	}
	fmt.Printf("PNG: %d bytes, decoded %dx%d px (head width_dots=%d, %s)\n",
		len(png), imgCfg.Width, imgCfg.Height, dev.WidthDots, compare(imgCfg.Width, dev.WidthDots))
	if imgCfg.Width != dev.WidthDots {
		outH := int(float64(imgCfg.Height)*float64(dev.WidthDots)/float64(imgCfg.Width) + 0.5)
		fmt.Printf("note: PNG %dx%d px resampled (aspect-preserving) to %dx%d dots (%.2gx supersample)\n",
			imgCfg.Width, imgCfg.Height, dev.WidthDots, outH, float64(imgCfg.Width)/float64(dev.WidthDots))
	}

	if err := os.WriteFile(*outPNG, png, 0o644); err == nil {
		fmt.Printf("saved PNG -> %s\n", *outPNG)
	}

	var buf bytes.Buffer
	opt := escpos.DefaultOptions(dev.WidthDots)
	if *band >= 0 {
		opt.BandHeight = *band
	}
	if err := escpos.Render(&buf, png, opt); err != nil {
		fmt.Fprintf(os.Stderr, "render escpos: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("rendered ESC/POS: %d bytes (%d bytes/row)\n", buf.Len(), (dev.WidthDots+7)/8)

	if err := os.WriteFile(*outRaw, buf.Bytes(), 0o644); err == nil {
		fmt.Printf("saved ESC/POS -> %s\n", *outRaw)
	}

	if *doPrint {
		if dev.Printer.Kind != config.KindCUPSRaw || dev.Printer.Queue == "" {
			fmt.Fprintf(os.Stderr, "--print only supports cups_raw with a queue (got kind=%q queue=%q)\n",
				dev.Printer.Kind, dev.Printer.Queue)
			os.Exit(1)
		}
		cmd := exec.Command("lp", "-d", dev.Printer.Queue, "-o", "raw")
		cmd.Stdin = bytes.NewReader(buf.Bytes())
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "lp to %s: %v: %s\n", dev.Printer.Queue, err, stderr.String())
			os.Exit(1)
		}
		fmt.Printf("sent to CUPS queue %q\n", dev.Printer.Queue)
	} else {
		fmt.Println("(dry run; pass --print to actually print)")
	}
}

func compare(imgW, headW int) string {
	switch {
	case imgW > headW:
		return fmt.Sprintf("WIDER by %d", imgW-headW)
	case imgW < headW:
		return fmt.Sprintf("narrower by %d", headW-imgW)
	default:
		return "matches head"
	}
}

// makeCalibPNG builds an n×n white image with black grid lines every 80 dots
// (10mm @ 203dpi) plus a full border and a diagonal. On a correctly-scaled
// printer every grid cell prints as a 10mm square; vertical compression shows
// up as cells shorter than they are wide.
func makeCalibPNG(n int) []byte {
	img := image.NewGray(image.Rect(0, 0, n, n))
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	black := color.Gray{Y: 0}
	set := func(x, y int) {
		if x >= 0 && x < n && y >= 0 && y < n {
			img.SetGray(x, y, black)
		}
	}
	for g := 0; g <= n; g += 80 {
		for k := 0; k < n; k++ {
			set(g, k)
			set(g-1, k)
			set(k, g)
			set(k, g-1)
		}
	}
	for k := 0; k < n; k++ {
		set(0, k)
		set(n-1, k)
		set(k, 0)
		set(k, n-1)
		set(k, k) // diagonal: 45° only if aspect is 1:1
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
