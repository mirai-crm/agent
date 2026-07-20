// Command agent is a cross-platform ESC/POS print agent for the CRM device API.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/mirai-agent/mirai-agent/internal/config"
	"github.com/mirai-agent/mirai-agent/internal/logx"
	"github.com/mirai-agent/mirai-agent/internal/version"
)

// Exit codes (spec §5.3).
const (
	exitOK           = 0
	exitGeneral      = 1
	exitUsage        = 2
	exitConfig       = 3
	exitBootstrap    = 4
	exitServicePerms = 5
	exitPrinterCheck = 6
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	// Top-level version/help.
	switch args[0] {
	case "--version", "-version", "version":
		fmt.Println("mirai-agent", version.Version)
		return exitOK
	case "--help", "-h", "help":
		usage()
		return exitOK
	}

	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "setup":
		return cmdSetup(rest)
	case "run":
		return cmdRun(rest)
	case "install":
		return cmdInstall(rest)
	case "uninstall":
		return cmdUninstall(rest)
	case "status":
		return cmdStatus(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mirai-agent — ESC/POS print agent for CRM

Usage:
  agent setup --api-url URL --token T1 [--token T2 ...] [--printer ref] [--no-service] [--yes]
  agent run [--config PATH] [--log-level LEVEL]
  agent install [--config PATH]
  agent uninstall [--config PATH]
  agent status [--config PATH]

Global flags:
  --config PATH        path to config.toml (default per-OS)
  --log-level LEVEL    trace|debug|info|warn|error
  --version, --help
`)
}

// stringSlice is a repeatable string flag.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// setupLogger builds a logger from config, overriding level if requested.
func setupLogger(cfg config.Config, levelOverride string) (*slog.Logger, func()) {
	lc := cfg.Log
	if levelOverride != "" {
		lc.Level = levelOverride
	}
	logger, closer, err := logx.Setup(lc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging setup failed: %v\n", err)
		logger = slog.Default()
	}
	return logger, func() {
		if closer != nil {
			_ = closer.Close()
		}
	}
}
