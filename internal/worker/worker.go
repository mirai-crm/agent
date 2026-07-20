// Package worker runs the per-device polling/print/finalize loop and heartbeat.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/config"
	"github.com/mirai-agent/mirai-agent/internal/escpos"
	"github.com/mirai-agent/mirai-agent/internal/logx"
	"github.com/mirai-agent/mirai-agent/internal/paymentjournal"
	"github.com/mirai-agent/mirai-agent/internal/printer"
	"github.com/mirai-agent/mirai-agent/internal/privatpos"
)

// Manager runs one deviceWorker per configured device.
type Manager struct {
	cfg     config.Config
	log     *slog.Logger
	journal *paymentjournal.Journal
}

// NewManager builds a Manager.
func NewManager(cfg config.Config, configPath string, log *slog.Logger) (*Manager, error) {
	m := &Manager{cfg: cfg, log: log}
	for _, dev := range cfg.Devices {
		if dev.Type != api.DeviceTypePOSTerminal {
			continue
		}
		journal, err := paymentjournal.Open(configPath + ".payments.json")
		if err != nil {
			return nil, err
		}
		m.journal = journal
		break
	}
	return m, nil
}

// Run starts all device workers and blocks until ctx is cancelled and all
// workers have drained.
func (m *Manager) Run(ctx context.Context) error {
	if len(m.cfg.Devices) == 0 {
		return errors.New("no devices configured")
	}
	var wg sync.WaitGroup
	for i := range m.cfg.Devices {
		dev := m.cfg.Devices[i]
		w, err := m.newDeviceWorker(dev)
		if err != nil {
			m.log.Error("skip device: cannot build worker",
				"device_id", dev.ID, "device_name", dev.Name, "error", err.Error())
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run(ctx)
		}()
	}
	wg.Wait()
	return nil
}

type deviceWorker struct {
	cfg      config.Config
	dev      config.DeviceConfig
	client   crmClient
	executor taskExecutor
	isPOS    bool
	journal  workerJournal
	log      *slog.Logger
}

type workerJournal interface {
	Complete(deviceID, taskID int64, data map[string]interface{}) error
	Pending(deviceID int64) []paymentjournal.Entry
	Remove(deviceID, taskID int64) error
}

func (m *Manager) newDeviceWorker(dev config.DeviceConfig) (*deviceWorker, error) {
	client := api.New(api.Config{
		BaseURL:         m.cfg.Server.BaseURL,
		Token:           dev.Token,
		RequestTimeout:  m.cfg.HTTP.RequestTimeout(),
		LongpollTimeout: m.cfg.HTTP.LongpollTimeout(),
	})
	log := m.log.With(
		"device_id", dev.ID,
		"device_name", dev.Name,
		"token", logx.TokenTag(dev.Token),
	)
	var executor taskExecutor
	switch dev.Type {
	case api.DeviceTypeReceiptPrinter:
		p, err := printer.New(dev.Printer)
		if err != nil {
			return nil, err
		}
		executor = &printerExecutor{dev: dev, client: client, printer: p}
	case api.DeviceTypeLabelPrinter:
		p, err := printer.New(dev.Printer)
		if err != nil {
			return nil, err
		}
		executor = newLabelExecutor(dev.Label, client, p)
	case api.DeviceTypePOSTerminal:
		if m.journal == nil {
			return nil, errors.New("payment journal is not initialized")
		}
		executor = newPOSExecutor(dev.ID, dev.Token, dev.POS.MerchantIDs, m.journal, privatpos.NewClient(
			dev.POS.Address,
			time.Duration(dev.POS.ConnectTimeoutSeconds)*time.Second,
			time.Duration(dev.POS.OperationTimeoutSeconds)*time.Second,
		))
	default:
		return nil, fmt.Errorf("unsupported device type %q", dev.Type)
	}
	return &deviceWorker{
		cfg:      m.cfg,
		dev:      dev,
		client:   client,
		executor: executor,
		isPOS:    dev.Type == api.DeviceTypePOSTerminal,
		journal:  m.journal,
		log:      log,
	}, nil
}

// run drives the poll loop plus a heartbeat ticker until ctx is cancelled.
func (w *deviceWorker) run(ctx context.Context) {
	w.log.Info("device worker started")
	defer w.log.Info("device worker stopped")
	defer w.executor.Close()
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.pingLoop(workerCtx)
	}()

	if w.isPOS && !w.replay(workerCtx) {
		cancel()
		wg.Wait()
		return
	}
	w.pollLoop(workerCtx)
	cancel()
	wg.Wait()
}

// pollLoop repeatedly long-polls for tasks and processes them.
func (w *deviceWorker) pollLoop(ctx context.Context) {
	netBackoff := w.cfg.Retry.NetworkBackoff()
	for {
		if ctx.Err() != nil {
			return
		}
		tasks, err := w.client.PollTasks(ctx, w.cfg.Poll.TimeoutSeconds, w.cfg.Poll.BatchSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			switch {
			case errors.Is(err, api.ErrUnauthorized):
				w.log.Error("token unauthorized/archived; stopping this device")
				return
			case errors.Is(err, api.ErrBadRequest):
				w.log.Error("tasks poll rejected as bad_request; backing off", "error", err.Error())
				if !sleepCtx(ctx, netBackoff) {
					return
				}
				netBackoff = capBackoff(netBackoff*2, w.cfg.Retry.NetworkBackoffMax())
			default:
				w.log.Warn("tasks poll failed; reconnecting", "error", err.Error(), "backoff", netBackoff.String())
				if !sleepCtx(ctx, netBackoff) {
					return
				}
				netBackoff = capBackoff(netBackoff*2, w.cfg.Retry.NetworkBackoffMax())
			}
			continue
		}
		// Success resets the network backoff.
		netBackoff = w.cfg.Retry.NetworkBackoff()

		if len(tasks) == 0 {
			// Normal long-poll timeout; immediately reopen.
			continue
		}
		for _, t := range tasks {
			if ctx.Err() != nil {
				return
			}
			w.processTask(ctx, t)
		}
	}
}

// processTask executes one task (with local retries) and finalizes it.
func (w *deviceWorker) processTask(ctx context.Context, t api.Task) {
	start := time.Now()
	log := w.log.With("task_id", t.ID, "task_name", t.Name)
	log.Info("processing task", "priority", t.Priority)

	var data map[string]interface{}
	var execErr error
	if w.isPOS {
		data, execErr = w.executor.Execute(ctx, t)
	} else {
		execErr = w.executeWithLocalRetry(ctx, t)
	}
	if ctx.Err() != nil {
		return
	}
	if isPaymentPersistenceError(execErr) {
		log.Error("payment result persistence failed; leaving task pending",
			"error", sanitizeError(execErr, w.dev.Token), "duration", time.Since(start).String())
		return
	}

	item := api.FinalizeItem{ID: t.ID}
	if execErr != nil {
		item.ErrorMessage = sanitizeError(execErr, w.dev.Token)
		log.Error("task failed; finalizing with error", "error", item.ErrorMessage, "duration", time.Since(start).String())
	} else {
		item.Data = data
		log.Info("task completed; finalizing", "duration", time.Since(start).String())
	}
	if w.finalize(ctx, item) && w.isPOS && item.Data != nil {
		if err := w.journal.Remove(w.dev.ID, t.ID); err != nil {
			w.log.Error("remove acknowledged payment journal entry", "task_id", t.ID, "error", err)
		}
	}
}

func (w *deviceWorker) finalize(ctx context.Context, item api.FinalizeItem) bool {
	resp, err := w.client.Finalize(ctx, []api.FinalizeItem{item})
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		w.log.Error("finalize failed", "task_id", item.ID, "error", sanitizeError(err, w.dev.Token))
		return false
	}
	w.log.Info("finalize result", "task_id", item.ID, "finalized", resp.Finalized, "skipped", resp.Skipped)
	return containsTask(resp.Finalized, item.ID) || containsTask(resp.Skipped, item.ID)
}

func (w *deviceWorker) replay(ctx context.Context) bool {
	for _, entry := range w.journal.Pending(w.dev.ID) {
		backoff := w.cfg.Retry.NetworkBackoff()
		for {
			if err := w.journal.Complete(w.dev.ID, entry.TaskID, entry.Data); err == nil {
				break
			} else {
				w.log.Error("re-persist replayed payment result failed",
					"task_id", entry.TaskID, "error", sanitizeError(err, w.dev.Token))
			}
			if ctx.Err() != nil || !sleepCtx(ctx, backoff) {
				return false
			}
			backoff = capBackoff(backoff*2, w.cfg.Retry.NetworkBackoffMax())
		}
		for !w.finalize(ctx, api.FinalizeItem{ID: entry.TaskID, Data: entry.Data}) {
			if ctx.Err() != nil || !sleepCtx(ctx, backoff) {
				return false
			}
			backoff = capBackoff(backoff*2, w.cfg.Retry.NetworkBackoffMax())
		}
		if err := w.journal.Remove(w.dev.ID, entry.TaskID); err != nil {
			w.log.Error("remove replayed payment journal entry", "task_id", entry.TaskID, "error", err)
			return false
		}
	}
	return true
}

func containsTask(ids []int64, id int64) bool {
	return slices.Contains(ids, id)
}

// executeWithLocalRetry retries transient errors with exponential backoff.
func (w *deviceWorker) executeWithLocalRetry(ctx context.Context, t api.Task) error {
	backoff := w.cfg.Retry.InitialBackoff()
	var lastErr error
	for attempt := 1; attempt <= w.cfg.Retry.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := w.executor.Execute(ctx, t)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isTransient(err) {
			return err
		}
		w.log.Warn("transient task error; will retry",
			"task_id", t.ID, "attempt", attempt, "max", w.cfg.Retry.MaxAttempts, "error", err.Error())
		if attempt < w.cfg.Retry.MaxAttempts {
			if !sleepCtx(ctx, jitter(backoff)) {
				return ctx.Err()
			}
			backoff = capBackoff(time.Duration(float64(backoff)*w.cfg.Retry.BackoffMultiplier), w.cfg.Retry.MaxBackoff())
		}
	}
	return fmt.Errorf("failed after %d attempts: %w", w.cfg.Retry.MaxAttempts, lastErr)
}

type taskExecutor interface {
	Execute(context.Context, api.Task) (map[string]interface{}, error)
	Close() error
}

type crmClient interface {
	PollTasks(context.Context, int, int) ([]api.Task, error)
	Finalize(context.Context, []api.FinalizeItem) (api.FinalizeResponse, error)
	Ping(context.Context) (api.PingResponse, error)
	FetchPNG(context.Context, string, int, int) ([]byte, error)
}

type printerExecutor struct {
	dev     config.DeviceConfig
	client  crmClient
	printer printer.Printer
}

func (e *printerExecutor) Close() error { return nil }

// Execute dispatches a task by name to the print pipeline.
func (e *printerExecutor) Execute(ctx context.Context, t api.Task) (map[string]interface{}, error) {
	switch t.Name {
	case api.TaskPrintCheck:
		var d api.PrintCheckData
		if err := json.Unmarshal(t.Data, &d); err != nil {
			return nil, permanent(fmt.Errorf("bad print_check data: %w", err))
		}
		if d.CheckID <= 0 {
			return nil, permanent(errors.New("print_check: checkId must be positive"))
		}
		return nil, e.printDocument(ctx, fmt.Sprintf("/api/v1/devices/checks/%d/png", d.CheckID))

	case api.TaskPrintZReport:
		var d api.PrintZReportData
		if err := json.Unmarshal(t.Data, &d); err != nil {
			return nil, permanent(fmt.Errorf("bad print_z_report data: %w", err))
		}
		if d.ZReportID <= 0 {
			return nil, permanent(errors.New("print_z_report: zReportId must be positive"))
		}
		return nil, e.printDocument(ctx, fmt.Sprintf("/api/v1/devices/z-reports/%d/png", d.ZReportID))

	default:
		return nil, permanent(fmt.Errorf("unsupported task name %q", t.Name))
	}
}

// printDocument downloads the PNG, renders ESC/POS, and writes it to the printer.
func (e *printerExecutor) printDocument(ctx context.Context, pngPath string) error {
	png, err := e.client.FetchPNG(ctx, pngPath, e.dev.PaperWidthMM(), e.dev.PNGScale)
	if err != nil {
		// 404 is permanent; other API errors classified by isTransient.
		return err
	}

	var buf bytes.Buffer
	opt := escpos.DefaultOptions(e.dev.WidthDots)
	if err := escpos.Render(&buf, png, opt); err != nil {
		// A decode/render failure on a fetched document is permanent.
		return permanent(err)
	}

	if err := e.printer.Open(ctx); err != nil {
		return fmt.Errorf("printer open: %w", err)
	}
	if err := printer.WriteChunked(e.printer, buf.Bytes()); err != nil {
		e.printer.Close()
		return fmt.Errorf("printer write: %w", err)
	}
	if err := e.printer.Close(); err != nil {
		return fmt.Errorf("printer close: %w", err)
	}
	return nil
}

// pingLoop sends a heartbeat every ping interval. Failures are logged as warn.
func (w *deviceWorker) pingLoop(ctx context.Context) {
	interval := w.cfg.Ping.Interval()
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := w.client.Ping(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, api.ErrUnauthorized) {
					w.log.Warn("ping unauthorized")
					continue
				}
				w.log.Warn("ping failed", "error", err.Error())
				continue
			}
			w.log.Debug("ping ok", "server_time", resp.ServerTime)
		}
	}
}

// ---- error classification helpers ----

// permanentError wraps an error to force non-retry.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }

// isTransient reports whether an error is worth retrying locally.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var perm *permanentError
	if errors.As(err, &perm) {
		return false
	}
	// Permanent API errors.
	if errors.Is(err, api.ErrNotFound) || errors.Is(err, api.ErrBadRequest) || errors.Is(err, api.ErrUnauthorized) {
		return false
	}
	// 5xx/429 API errors are transient.
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Transient()
	}
	// Network errors, printer busy, timeouts, PNG download issues: transient.
	return true
}

// sanitizeError builds an error_message safe to send to the server: never
// includes the secret token.
func sanitizeError(err error, token string) string {
	msg := err.Error()
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "[redacted]")
	}
	return msg
}

// ---- timing helpers ----

func capBackoff(d, max time.Duration) time.Duration {
	if max > 0 && d > max {
		return max
	}
	if d <= 0 {
		return 100 * time.Millisecond
	}
	return d
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// +/- 20% jitter to avoid thundering herds.
	delta := time.Duration(rand.Int63n(int64(d)/5+1)) - d/10
	return d + delta
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
