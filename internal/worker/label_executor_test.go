package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/config"
)

func TestManagerBuildsLabelExecutorForLabelPrinter(t *testing.T) {
	cfg := config.Default()
	cfg.Server.BaseURL = "https://crm.example.com"
	manager := &Manager{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dev := config.DeviceConfig{
		Token: "label-token",
		ID:    9,
		Name:  "Labels",
		Type:  api.DeviceTypeLabelPrinter,
		Printer: config.PrinterConfig{
			Kind: config.KindUSB, VendorID: "0x1234", ProductID: "0x5678",
		},
		Label: config.LabelConfig{DPI: 203, GapMM: 2},
	}

	worker, err := manager.newDeviceWorker(dev)
	if err != nil && strings.Contains(err.Error(), "requires a cgo build") {
		t.Skip("direct USB backend is unavailable in this build")
	}
	if err != nil {
		t.Fatalf("newDeviceWorker() error = %v", err)
	}
	if _, ok := worker.executor.(*labelExecutor); !ok {
		t.Fatalf("executor type = %T", worker.executor)
	}
}

func TestLabelExecutorAppliesDefaultsAndPrintsBatch(t *testing.T) {
	client := &stubLabelClient{png: labelPNG(t)}
	output := &stubLabelPrinter{}
	executor := newLabelExecutor(config.LabelConfig{DPI: 203, GapMM: 2}, client, output)

	_, err := executor.Execute(context.Background(), labelTask(t, map[string]any{
		"nomenclatureIds": []int64{11, 22},
	}))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(client.calls) != 2 || client.calls[0].id != 11 || client.calls[1].id != 22 {
		t.Fatalf("fetch calls = %#v", client.calls)
	}
	for _, call := range client.calls {
		opt := call.opt
		if !opt.Name || !opt.Price || !opt.Barcode || opt.WidthMM != 58 || opt.HeightMM != 40 || opt.Scale != nil {
			t.Fatalf("default options = %+v", opt)
		}
	}
	if output.opens != 1 || output.closes != 1 {
		t.Fatalf("printer open/close = %d/%d", output.opens, output.closes)
	}
	if got := bytes.Count(output.data.Bytes(), []byte("PRINT 1,1\r\n")); got != 2 {
		t.Fatalf("PRINT command count = %d, want 2", got)
	}
}

func TestLabelExecutorMakesWriteFailurePermanent(t *testing.T) {
	client := &stubLabelClient{png: labelPNG(t)}
	output := &stubLabelPrinter{failAtWrite: 2}
	executor := newLabelExecutor(config.LabelConfig{DPI: 203, GapMM: 2}, client, output)

	_, err := executor.Execute(context.Background(), labelTask(t, map[string]any{
		"nomenclatureIds": []int64{11, 22},
		"widthMm":         20,
		"heightMm":        10,
	}))
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	if isTransient(err) {
		t.Fatalf("write error is transient: %v", err)
	}
	if len(client.calls) != 2 {
		t.Fatalf("fetch calls = %d, want complete prefetch before printing", len(client.calls))
	}
}

func TestLabelExecutorRejectsInvalidPayloadBeforeFetch(t *testing.T) {
	client := &stubLabelClient{png: labelPNG(t)}
	executor := newLabelExecutor(
		config.LabelConfig{DPI: 203, GapMM: 2},
		client,
		&stubLabelPrinter{},
	)

	for name, data := range map[string]map[string]any{
		"empty ids":    {"nomenclatureIds": []int64{}},
		"invalid id":   {"nomenclatureIds": []int64{0}},
		"narrow label": {"nomenclatureIds": []int64{1}, "widthMm": 19},
		"tall label":   {"nomenclatureIds": []int64{1}, "heightMm": 121},
		"bad scale":    {"nomenclatureIds": []int64{1}, "scale": 5},
		"null flag":    {"nomenclatureIds": []int64{1}, "name": nil},
		"unknown key":  {"nomenclatureIds": []int64{1}, "extra": true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := executor.Execute(context.Background(), labelTask(t, data)); err == nil {
				t.Fatal("Execute() error = nil")
			}
		})
	}
	if len(client.calls) != 0 {
		t.Fatalf("fetch calls = %d", len(client.calls))
	}
}

func labelTask(t *testing.T, data map[string]any) api.Task {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return api.Task{ID: 7, Name: api.TaskPrintLabel, Data: raw}
}

func labelPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.Black)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type labelFetchCall struct {
	id  int64
	opt api.LabelPNGOptions
}

type stubLabelClient struct {
	png   []byte
	err   error
	calls []labelFetchCall
}

func (c *stubLabelClient) FetchLabelPNG(_ context.Context, id int64, opt api.LabelPNGOptions) ([]byte, error) {
	c.calls = append(c.calls, labelFetchCall{id: id, opt: opt})
	return c.png, c.err
}

type stubLabelPrinter struct {
	data        bytes.Buffer
	opens       int
	closes      int
	writes      int
	failAtWrite int
}

func (p *stubLabelPrinter) Open(context.Context) error {
	p.opens++
	return nil
}

func (p *stubLabelPrinter) Write(data []byte) (int, error) {
	p.writes++
	if p.failAtWrite > 0 && p.writes == p.failAtWrite {
		return 0, errors.New("usb write failed")
	}
	return p.data.Write(data)
}

func (p *stubLabelPrinter) Close() error {
	p.closes++
	return nil
}
