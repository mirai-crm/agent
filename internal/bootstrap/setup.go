// Package bootstrap implements the `agent setup` first-run flow: token
// self-discovery, printer binding, config persistence and service install.
package bootstrap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/config"
	"github.com/mirai-agent/mirai-agent/internal/printer"
)

// Exit codes surfaced to the CLI (mirrors spec §5.3).
const (
	ExitBootstrapFailed = 4
)

// ErrNoValidTokens indicates no token passed self-discovery (exit code 4).
var ErrNoValidTokens = errors.New("no valid tokens: /info returned 401 or unsupported device type for all tokens")

// Options configures the setup flow.
type Options struct {
	APIURL        string            // --api-url (may be empty; falls back to config)
	Tokens        []string          // --token (repeated) / positional
	PrinterBinds  map[string]string // deviceRef -> printerRef (from --printer)
	TerminalBinds map[string]string // deviceRef -> host:port (from --terminal)
	NoService     bool              // --no-service
	Yes           bool              // --yes (non-interactive)
	ConfigPath    string            // resolved config path
	RequestTO     time.Duration
	In            io.Reader // stdin (for prompts)
	Out           io.Writer // stdout (for prompts)
}

// InstallFunc installs+starts the OS service (injected so setup does not depend
// on the svc package build tags directly).
type InstallFunc func(configPath string) error

// Result summarises a completed setup.
type Result struct {
	BaseURL     string
	Devices     []config.DeviceConfig
	SkippedToks int
}

// Run performs the full setup flow and returns the resolved config on success.
func Run(ctx context.Context, opt Options, install InstallFunc) (Result, error) {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	// Load existing config (if any) to merge base_url and existing devices.
	existing, _, err := config.LoadRaw(opt.ConfigPath)
	if err != nil {
		return Result{}, fmt.Errorf("load existing config: %w", err)
	}

	baseURL := resolveBaseURL(opt.APIURL, existing.Server.BaseURL)
	if baseURL == "" {
		if opt.Yes {
			return Result{}, fmt.Errorf("--api-url is required (no base_url in config and --yes given)")
		}
		baseURL, err = promptLine(opt.In, opt.Out, "Enter CRM API base URL (e.g. https://crm.example.com): ")
		if err != nil {
			return Result{}, err
		}
		baseURL = strings.TrimSpace(baseURL)
		if baseURL == "" {
			return Result{}, errors.New("api base URL is required")
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")

	if len(opt.Tokens) == 0 {
		return Result{}, errors.New("at least one --token is required")
	}

	cfg := existing
	cfg.Server.BaseURL = baseURL

	// Index existing devices by token so re-running setup updates in place.
	byToken := map[string]int{}
	for i, d := range cfg.Devices {
		byToken[d.Token] = i
	}

	var accepted int
	var skipped int
	for _, token := range opt.Tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		client := api.New(api.Config{BaseURL: baseURL, Token: token, RequestTimeout: opt.RequestTO})
		info, err := client.Info(ctx)
		if err != nil {
			if errors.Is(err, api.ErrUnauthorized) {
				fmt.Fprintf(opt.Out, "token skipped: unauthorized or archived device\n")
			} else {
				fmt.Fprintf(opt.Out, "token skipped: /info failed: %v\n", err)
			}
			skipped++
			continue
		}
		if info.Type != api.DeviceTypeReceiptPrinter &&
			info.Type != api.DeviceTypeLabelPrinter &&
			info.Type != api.DeviceTypePOSTerminal {
			fmt.Fprintf(opt.Out, "token skipped: device %q has unsupported type %q (need %s)\n",
				info.Name, info.Type,
				api.DeviceTypeReceiptPrinter+"|"+api.DeviceTypeLabelPrinter+"|"+api.DeviceTypePOSTerminal)
			skipped++
			continue
		}

		dev := config.DeviceConfig{
			Token: token,
			ID:    info.ID,
			Name:  info.Name,
			Type:  info.Type,
		}
		switch info.Type {
		case api.DeviceTypeReceiptPrinter:
			pc, width, err := opt.resolvePrinter(info, true)
			if err != nil {
				fmt.Fprintf(opt.Out, "token skipped: printer binding failed for %q: %v\n", info.Name, err)
				skipped++
				continue
			}
			dev.WidthDots = width
			dev.Printer = pc
		case api.DeviceTypeLabelPrinter:
			pc, _, err := opt.resolvePrinter(info, false)
			if err != nil {
				fmt.Fprintf(opt.Out, "token skipped: printer binding failed for %q: %v\n", info.Name, err)
				skipped++
				continue
			}
			dev.Printer = pc
			dev.Label = config.LabelConfig{DPI: 203, GapMM: 2}
			if idx, ok := byToken[token]; ok && cfg.Devices[idx].Type == api.DeviceTypeLabelPrinter {
				dev.Label = cfg.Devices[idx].Label
			}
		case api.DeviceTypePOSTerminal:
			pos, err := opt.resolvePOS(info)
			if err != nil {
				fmt.Fprintf(opt.Out, "token skipped: terminal binding failed for %q: %v\n", info.Name, err)
				skipped++
				continue
			}
			if idx, ok := byToken[token]; ok {
				pos.MerchantIDs = cfg.Devices[idx].POS.MerchantIDs
			}
			dev.POS = pos
		}

		// Optional test print (interactive only).
		if info.Type != api.DeviceTypePOSTerminal && !opt.Yes {
			if yes, _ := promptYesNo(opt.In, opt.Out, fmt.Sprintf("Run a test print on %q?", info.Name)); yes {
				if err := TestPrint(ctx, dev.Type, dev.Printer, dev.Label); err != nil {
					fmt.Fprintf(opt.Out, "test print failed: %v\n", err)
				} else {
					fmt.Fprintf(opt.Out, "test print sent.\n")
				}
			}
		}

		if idx, ok := byToken[token]; ok {
			cfg.Devices[idx] = dev
		} else {
			cfg.Devices = append(cfg.Devices, dev)
			byToken[token] = len(cfg.Devices) - 1
		}
		accepted++
		fmt.Fprintf(opt.Out, "device configured: id=%d name=%q type=%s\n", info.ID, info.Name, info.Type)
	}

	if accepted == 0 {
		return Result{SkippedToks: skipped}, ErrNoValidTokens
	}

	if err := config.Save(opt.ConfigPath, cfg); err != nil {
		return Result{}, fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(opt.Out, "config written to %s (0600)\n", opt.ConfigPath)

	if !opt.NoService && install != nil {
		if err := install(opt.ConfigPath); err != nil {
			return Result{BaseURL: baseURL, Devices: cfg.Devices, SkippedToks: skipped},
				fmt.Errorf("install service: %w", err)
		}
		fmt.Fprintf(opt.Out, "service installed and started\n")
	}

	return Result{BaseURL: baseURL, Devices: cfg.Devices, SkippedToks: skipped}, nil
}

func resolveBaseURL(flagURL, configURL string) string {
	if strings.TrimSpace(flagURL) != "" {
		return strings.TrimSpace(flagURL)
	}
	return strings.TrimSpace(configURL)
}

// resolvePrinter determines the printer binding for a device, from --printer if
// present, otherwise interactively.
func (opt Options) resolvePrinter(info api.DeviceInfo, askWidth bool) (config.PrinterConfig, int, error) {
	width := defaultWidthDots

	if ref, ok := opt.printerRefFor(info); ok {
		pc, err := parsePrinterRef(ref)
		if err != nil {
			return config.PrinterConfig{}, 0, err
		}
		return pc, width, nil
	}

	if opt.Yes {
		return config.PrinterConfig{}, 0, errors.New("no --printer binding provided and --yes given")
	}
	pc, err := opt.interactivePrinter(info)
	if err != nil {
		return config.PrinterConfig{}, 0, err
	}
	if askWidth {
		if w, err := promptWidth(opt.In, opt.Out); err == nil && w > 0 {
			width = w
		}
	}
	return pc, width, nil
}

const defaultWidthDots = 576

// printerRefFor looks up a --printer binding by device id or name.
func (opt Options) printerRefFor(info api.DeviceInfo) (string, bool) {
	if opt.PrinterBinds == nil {
		return "", false
	}
	if ref, ok := opt.PrinterBinds[strconv.FormatInt(info.ID, 10)]; ok {
		return ref, true
	}
	if ref, ok := opt.PrinterBinds[info.Name]; ok {
		return ref, true
	}
	return "", false
}

func (opt Options) terminalRefFor(info api.DeviceInfo) (string, bool) {
	if opt.TerminalBinds == nil {
		return "", false
	}
	if ref, ok := opt.TerminalBinds[strconv.FormatInt(info.ID, 10)]; ok {
		return ref, true
	}
	if ref, ok := opt.TerminalBinds[info.Name]; ok {
		return ref, true
	}
	return "", false
}

func (opt Options) resolvePOS(info api.DeviceInfo) (config.POSConfig, error) {
	if ref, ok := opt.terminalRefFor(info); ok {
		ref = strings.TrimSpace(ref)
		if err := config.ValidatePOSAddress(ref); err != nil {
			return config.POSConfig{}, fmt.Errorf("invalid terminal address %q: %w", ref, err)
		}
		return config.POSConfig{
			Address:                 ref,
			ConnectTimeoutSeconds:   5,
			OperationTimeoutSeconds: 180,
		}, nil
	}
	if opt.Yes {
		return config.POSConfig{}, errors.New("no --terminal binding provided and --yes given")
	}
	addr, err := promptLine(opt.In, opt.Out, "Enter terminal address (host:port): ")
	if err != nil {
		return config.POSConfig{}, err
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return config.POSConfig{}, errors.New("terminal address is required")
	}
	if err := config.ValidatePOSAddress(addr); err != nil {
		return config.POSConfig{}, fmt.Errorf("invalid terminal address %q: %w", addr, err)
	}
	return config.POSConfig{
		Address:                 addr,
		ConnectTimeoutSeconds:   5,
		OperationTimeoutSeconds: 180,
	}, nil
}

// interactivePrinter shows discovered options and reads the operator's choice.
func (opt Options) interactivePrinter(info api.DeviceInfo) (config.PrinterConfig, error) {
	options := discoverPrinters()
	fmt.Fprintf(opt.Out, "\nSelect a printer for device %q (id=%d):\n", info.Name, info.ID)
	for i, o := range options {
		fmt.Fprintf(opt.Out, "  [%d] %s\n", i+1, o.Label)
	}
	fmt.Fprintf(opt.Out, "  [m] manual entry (kind:args, e.g. dev_lp:/dev/usb/lp0, cups_raw:queue, windows_spooler:Name, usb:0x0416:0x5011)\n")
	choice, err := promptLine(opt.In, opt.Out, "Choice: ")
	if err != nil {
		return config.PrinterConfig{}, err
	}
	choice = strings.TrimSpace(choice)
	if strings.EqualFold(choice, "m") || choice == "" {
		ref, err := promptLine(opt.In, opt.Out, "Enter printer ref (kind:args): ")
		if err != nil {
			return config.PrinterConfig{}, err
		}
		return parsePrinterRef(strings.TrimSpace(ref))
	}
	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > len(options) {
		return config.PrinterConfig{}, fmt.Errorf("invalid choice %q", choice)
	}
	return options[n-1].Config, nil
}

// parsePrinterRef parses "kind:args" into a PrinterConfig.
//
//	dev_lp:/dev/usb/lp0
//	cups_raw:thermal_raw
//	windows_spooler:XP-58 (RAW)
//	usb:0x0416:0x5011[:serial]
func parsePrinterRef(ref string) (config.PrinterConfig, error) {
	ref = strings.TrimSpace(ref)
	kind, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return config.PrinterConfig{}, fmt.Errorf("invalid printer ref %q (want kind:args)", ref)
	}
	switch kind {
	case config.KindDevLP:
		return config.PrinterConfig{Kind: kind, Path: rest}, nil
	case config.KindCUPSRaw:
		return config.PrinterConfig{Kind: kind, Queue: rest}, nil
	case config.KindWindowsSpooler:
		return config.PrinterConfig{Kind: kind, SpoolerName: rest}, nil
	case config.KindUSB:
		parts := strings.Split(rest, ":")
		if len(parts) < 2 {
			return config.PrinterConfig{}, fmt.Errorf("usb ref needs vendor:product[:serial]")
		}
		pc := config.PrinterConfig{Kind: kind, VendorID: parts[0], ProductID: parts[1]}
		if len(parts) >= 3 {
			pc.Serial = parts[2]
		}
		return pc, nil
	default:
		return config.PrinterConfig{}, fmt.Errorf("unknown printer kind %q", kind)
	}
}

// TestPrint opens the bound printer and sends a protocol-appropriate test job.
func TestPrint(ctx context.Context, deviceType string, pc config.PrinterConfig, label config.LabelConfig) error {
	p, err := printer.New(pc)
	if err != nil {
		return err
	}
	if err := p.Open(ctx); err != nil {
		return err
	}
	var data []byte
	switch deviceType {
	case api.DeviceTypeReceiptPrinter:
		// ESC @ init, text "mirai-agent OK", feed, partial cut.
		data = []byte{0x1B, 0x40}
		data = append(data, []byte("mirai-agent test print OK\n")...)
		data = append(data, []byte{0x0A, 0x0A, 0x0A, 0x0A}...)
		data = append(data, []byte{0x1D, 0x56, 0x01}...)
	case api.DeviceTypeLabelPrinter:
		data = []byte(fmt.Sprintf(
			"SIZE 58 mm,40 mm\r\nGAP %g mm,%g mm\r\nCLS\r\nTEXT 20,20,\"0\",0,1,1,\"mirai-agent OK\"\r\nPRINT 1,1\r\n",
			label.GapMM, label.GapOffsetMM,
		))
	default:
		_ = p.Close()
		return fmt.Errorf("test print is unsupported for device type %q", deviceType)
	}
	if err := printer.WriteChunked(p, data); err != nil {
		p.Close()
		return err
	}
	return p.Close()
}

// ---- prompt helpers ----

func promptLine(in io.Reader, out io.Writer, prompt string) (string, error) {
	if in == nil {
		return "", errors.New("no input available for prompt")
	}
	fmt.Fprint(out, prompt)
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func promptYesNo(in io.Reader, out io.Writer, prompt string) (bool, error) {
	ans, err := promptLine(in, out, prompt+" [y/N]: ")
	if err != nil {
		return false, err
	}
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes", nil
}

func promptWidth(in io.Reader, out io.Writer) (int, error) {
	ans, err := promptLine(in, out, fmt.Sprintf("Print width in dots [%d] (58mm=384, 80mm=576): ", defaultWidthDots))
	if err != nil {
		return 0, err
	}
	ans = strings.TrimSpace(ans)
	if ans == "" {
		return defaultWidthDots, nil
	}
	return strconv.Atoi(ans)
}
