package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mirai-agent/mirai-agent/internal/bootstrap"
	"github.com/mirai-agent/mirai-agent/internal/config"
	"github.com/mirai-agent/mirai-agent/internal/logx"
	"github.com/mirai-agent/mirai-agent/internal/svc"
)

// commonFlags registers --config and --log-level on a FlagSet.
func commonFlags(fs *flag.FlagSet) (configPath, logLevel *string) {
	configPath = fs.String("config", config.DefaultPath(), "path to config.toml")
	logLevel = fs.String("log-level", "", "override log level: trace|debug|info|warn|error")
	return
}

func cmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	configPath, logLevel := commonFlags(fs)
	apiURL := fs.String("api-url", "", "CRM API base URL (required on first setup)")
	var tokens stringSlice
	fs.Var(&tokens, "token", "device secret token (repeatable)")
	var printers stringSlice
	fs.Var(&printers, "printer", "device binding deviceRef=printerRef (repeatable)")
	noService := fs.Bool("no-service", false, "do not install the OS service")
	yes := fs.Bool("yes", false, "non-interactive; fail instead of prompting")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// Positional args are additional tokens.
	tokens = append(tokens, fs.Args()...)

	if len(tokens) == 0 {
		fmt.Fprintln(os.Stderr, "setup: at least one --token (or positional token) is required")
		return exitUsage
	}

	binds, err := parsePrinterBinds(printers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		return exitUsage
	}

	// A minimal logger for the install step.
	logger, closeLog := setupLogger(config.Default(), *logLevel)
	defer closeLog()

	install := func(cfgPath string) error {
		return svc.Install(cfgPath, logger)
	}

	opt := bootstrap.Options{
		APIURL:       *apiURL,
		Tokens:       tokens,
		PrinterBinds: binds,
		NoService:    *noService,
		Yes:          *yes,
		ConfigPath:   *configPath,
		RequestTO:    20 * time.Second,
		In:           os.Stdin,
		Out:          os.Stdout,
	}

	res, err := bootstrap.Run(context.Background(), opt, install)
	if err != nil {
		if errors.Is(err, bootstrap.ErrNoValidTokens) {
			fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
			return exitBootstrap
		}
		var permErr *svc.PermissionError
		if errors.As(err, &permErr) {
			fmt.Fprintf(os.Stderr, "setup: service install needs admin/root: %v\n", err)
			return exitServicePerms
		}
		fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
		return exitGeneral
	}
	fmt.Printf("setup complete: %d device(s) configured, %d token(s) skipped\n", len(res.Devices), res.SkippedToks)
	return exitOK
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath, logLevel := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return exitConfig
	}
	logger, closeLog := setupLogger(cfg, *logLevel)
	defer closeLog()

	logger.Info("starting agent", "version", Version, "config", *configPath, "devices", len(cfg.Devices))
	if err := svc.Run(cfg, *configPath, logger); err != nil {
		logger.Error("run exited with error", "error", err.Error())
		return exitGeneral
	}
	return exitOK
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	configPath, logLevel := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// Config must exist/validate before installing a service that runs it.
	if _, err := config.Load(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return exitConfig
	}
	logger, closeLog := setupLogger(config.Default(), *logLevel)
	defer closeLog()

	if err := svc.Install(*configPath, logger); err != nil {
		return serviceErrExit(err, "install")
	}
	fmt.Println("service installed and started")
	return exitOK
}

func cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	configPath, logLevel := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	logger, closeLog := setupLogger(config.Default(), *logLevel)
	defer closeLog()

	if err := svc.Uninstall(*configPath, logger); err != nil {
		return serviceErrExit(err, "uninstall")
	}
	fmt.Println("service uninstalled")
	return exitOK
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath, _ := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	cfg, existed, err := config.LoadRaw(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return exitConfig
	}
	fmt.Printf("config path:  %s\n", *configPath)
	if !existed {
		fmt.Println("config:       NOT FOUND")
	} else {
		fmt.Printf("base_url:     %s\n", cfg.Server.BaseURL)
		fmt.Printf("devices:      %d\n", len(cfg.Devices))
		for _, d := range cfg.Devices {
			fmt.Printf("  - id=%d name=%q width=%d printer=%s token=%s\n",
				d.ID, d.Name, d.WidthDots, printerSummary(d.Printer), logx.TokenTag(d.Token))
		}
	}
	st, err := svc.Status(*configPath)
	if err != nil {
		fmt.Printf("service:      %s (%v)\n", st, err)
	} else {
		fmt.Printf("service:      %s\n", st)
	}
	return exitOK
}

func printerSummary(p config.PrinterConfig) string {
	switch p.Kind {
	case config.KindWindowsSpooler:
		return "windows_spooler(" + p.SpoolerName + ")"
	case config.KindCUPSRaw:
		return "cups_raw(" + p.Queue + ")"
	case config.KindDevLP:
		return "dev_lp(" + p.Path + ")"
	case config.KindUSB:
		return "usb(" + p.VendorID + ":" + p.ProductID + ")"
	default:
		return p.Kind
	}
}

func serviceErrExit(err error, op string) int {
	var permErr *svc.PermissionError
	if errors.As(err, &permErr) {
		fmt.Fprintf(os.Stderr, "%s: needs admin/root privileges: %v\n", op, err)
		return exitServicePerms
	}
	fmt.Fprintf(os.Stderr, "%s failed: %v\n", op, err)
	return exitServicePerms
}

// parsePrinterBinds turns []"deviceRef=printerRef" into a map.
func parsePrinterBinds(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		ref, val, ok := strings.Cut(p, "=")
		if !ok || ref == "" || val == "" {
			return nil, fmt.Errorf("invalid --printer %q (want deviceRef=printerRef)", p)
		}
		out[ref] = val
	}
	return out, nil
}
