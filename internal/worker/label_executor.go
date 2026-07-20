package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/config"
	"github.com/mirai-agent/mirai-agent/internal/printer"
	"github.com/mirai-agent/mirai-agent/internal/tspl"
)

type labelClient interface {
	FetchLabelPNG(context.Context, int64, api.LabelPNGOptions) ([]byte, error)
}

type labelExecutor struct {
	cfg     config.LabelConfig
	client  labelClient
	printer printer.Printer
}

func newLabelExecutor(cfg config.LabelConfig, client labelClient, p printer.Printer) *labelExecutor {
	return &labelExecutor{cfg: cfg, client: client, printer: p}
}

func (e *labelExecutor) Close() error { return nil }

func (e *labelExecutor) Execute(ctx context.Context, task api.Task) (map[string]interface{}, error) {
	if task.Name != api.TaskPrintLabel {
		return nil, permanent(fmt.Errorf("unsupported task name %q", task.Name))
	}
	input, opt, err := decodeLabelTask(task.Data)
	if err != nil {
		return nil, permanent(err)
	}

	jobs := make([][]byte, 0, len(input.NomenclatureIDs))
	for _, id := range input.NomenclatureIDs {
		pngData, err := e.client.FetchLabelPNG(ctx, id, opt)
		if err != nil {
			return nil, fmt.Errorf("fetch label %d: %w", id, err)
		}
		var job bytes.Buffer
		if err := tspl.Render(&job, pngData, tspl.Options{
			WidthMM:     opt.WidthMM,
			HeightMM:    opt.HeightMM,
			DPI:         e.cfg.DPI,
			GapMM:       e.cfg.GapMM,
			GapOffsetMM: e.cfg.GapOffsetMM,
			Threshold:   128,
		}); err != nil {
			return nil, permanent(fmt.Errorf("render label %d: %w", id, err))
		}
		jobs = append(jobs, job.Bytes())
	}

	if err := e.printer.Open(ctx); err != nil {
		return nil, fmt.Errorf("label printer open: %w", err)
	}
	for i, job := range jobs {
		if err := printer.WriteChunked(e.printer, job); err != nil {
			_ = e.printer.Close()
			return nil, permanent(fmt.Errorf("label %d printer write: %w", input.NomenclatureIDs[i], err))
		}
	}
	if err := e.printer.Close(); err != nil {
		return nil, permanent(fmt.Errorf("label printer close: %w", err))
	}
	return nil, nil
}

func decodeLabelTask(raw []byte) (api.PrintLabelData, api.LabelPNGOptions, error) {
	var input api.PrintLabelData
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err == nil {
		for _, name := range []string{"name", "price", "barcode", "widthMm", "heightMm", "scale"} {
			if value, ok := fields[name]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return input, api.LabelPNGOptions{}, fmt.Errorf("bad print_label data: %s must not be null", name)
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, api.LabelPNGOptions{}, fmt.Errorf("bad print_label data: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return input, api.LabelPNGOptions{}, errors.New("bad print_label data: trailing JSON value")
	}
	if len(input.NomenclatureIDs) == 0 {
		return input, api.LabelPNGOptions{}, errors.New("print_label: nomenclatureIds must not be empty")
	}
	for _, id := range input.NomenclatureIDs {
		if id <= 0 {
			return input, api.LabelPNGOptions{}, errors.New("print_label: nomenclatureIds must contain positive integers")
		}
	}

	opt := api.LabelPNGOptions{
		Name:     boolDefault(input.Name, true),
		Price:    boolDefault(input.Price, true),
		Barcode:  boolDefault(input.Barcode, true),
		WidthMM:  floatDefault(input.WidthMM, 58),
		HeightMM: floatDefault(input.HeightMM, 40),
		Scale:    input.Scale,
	}
	if opt.WidthMM < 20 || opt.WidthMM > 120 {
		return input, opt, errors.New("print_label: widthMm must be between 20 and 120")
	}
	if opt.HeightMM < 10 || opt.HeightMM > 120 {
		return input, opt, errors.New("print_label: heightMm must be between 10 and 120")
	}
	if opt.Scale != nil && (*opt.Scale < 1 || *opt.Scale > 4) {
		return input, opt, errors.New("print_label: scale must be between 1 and 4")
	}
	return input, opt, nil
}

func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func floatDefault(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}
