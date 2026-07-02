// Package config loads and persists the agent's TOML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the root of config.toml.
type Config struct {
	Server  ServerConfig   `toml:"server"`
	Poll    PollConfig     `toml:"poll"`
	Ping    PingConfig     `toml:"ping"`
	HTTP    HTTPConfig     `toml:"http"`
	Retry   RetryConfig    `toml:"retry"`
	Log     LogConfig      `toml:"log"`
	Devices []DeviceConfig `toml:"devices"`
}

// ServerConfig holds the CRM base URL.
type ServerConfig struct {
	BaseURL string `toml:"base_url"`
}

// PollConfig controls long-poll behaviour for GET /tasks.
type PollConfig struct {
	TimeoutSeconds int `toml:"timeout_seconds"`
	BatchSize      int `toml:"batch_size"`
}

// PingConfig controls the presence heartbeat.
type PingConfig struct {
	IntervalSeconds int `toml:"interval_seconds"`
}

// HTTPConfig controls HTTP client timeouts.
type HTTPConfig struct {
	RequestTimeoutSeconds  int `toml:"request_timeout_seconds"`
	LongpollTimeoutSeconds int `toml:"longpoll_timeout_seconds"`
}

// RetryConfig is the local retry/backoff policy (no server-side retries exist).
type RetryConfig struct {
	MaxAttempts         int     `toml:"max_attempts"`
	InitialBackoffMs    int     `toml:"initial_backoff_ms"`
	MaxBackoffMs        int     `toml:"max_backoff_ms"`
	BackoffMultiplier   float64 `toml:"backoff_multiplier"`
	NetworkBackoffMs    int     `toml:"network_backoff_ms"`
	NetworkBackoffMaxMs int     `toml:"network_backoff_max_ms"`
}

// LogConfig controls logging output.
type LogConfig struct {
	Level string `toml:"level"`
	Path  string `toml:"path"`
}

// DeviceConfig is one logical device = one secretToken = one physical printer.
type DeviceConfig struct {
	Token     string        `toml:"token"`
	ID        int64         `toml:"id"`
	Name      string        `toml:"name"`
	WidthDots int           `toml:"width_dots"`
	PNGScale  int           `toml:"png_scale"`
	Printer   PrinterConfig `toml:"printer"`
}

// PrinterConfig binds a device to a physical printer. Exactly one kind's fields apply.
type PrinterConfig struct {
	Kind        string `toml:"kind"` // windows_spooler | cups_raw | dev_lp | usb
	SpoolerName string `toml:"spooler_name,omitempty"`
	Queue       string `toml:"queue,omitempty"`
	Path        string `toml:"path,omitempty"`
	VendorID    string `toml:"vendor_id,omitempty"`
	ProductID   string `toml:"product_id,omitempty"`
	Serial      string `toml:"serial,omitempty"`
}

// Printer kinds.
const (
	KindWindowsSpooler = "windows_spooler"
	KindCUPSRaw        = "cups_raw"
	KindDevLP          = "dev_lp"
	KindUSB            = "usb"
)

// Supported paper widths in millimetres (server ?width param): 58mm → 384 dots,
// 80mm → 576 dots.
const (
	PaperWidth58mm = 58
	PaperWidth80mm = 80
)

// PaperWidthMM maps the printer head width in dots to the paper width in mm
// expected by the server's ?width query parameter. Anything wide enough for an
// 80mm head (≈576 dots) reports 80; everything else reports 58.
func (d DeviceConfig) PaperWidthMM() int {
	// Midpoint between the 58mm (384) and 80mm (576) native widths.
	if d.WidthDots >= 480 {
		return PaperWidth80mm
	}
	return PaperWidth58mm
}

// Durations derived from the numeric config fields.

func (p PollConfig) Timeout() time.Duration { return time.Duration(p.TimeoutSeconds) * time.Second }
func (p PingConfig) Interval() time.Duration {
	return time.Duration(p.IntervalSeconds) * time.Second
}
func (h HTTPConfig) RequestTimeout() time.Duration {
	return time.Duration(h.RequestTimeoutSeconds) * time.Second
}
func (h HTTPConfig) LongpollTimeout() time.Duration {
	return time.Duration(h.LongpollTimeoutSeconds) * time.Second
}
func (r RetryConfig) InitialBackoff() time.Duration {
	return time.Duration(r.InitialBackoffMs) * time.Millisecond
}
func (r RetryConfig) MaxBackoff() time.Duration {
	return time.Duration(r.MaxBackoffMs) * time.Millisecond
}
func (r RetryConfig) NetworkBackoff() time.Duration {
	return time.Duration(r.NetworkBackoffMs) * time.Millisecond
}
func (r RetryConfig) NetworkBackoffMax() time.Duration {
	return time.Duration(r.NetworkBackoffMaxMs) * time.Millisecond
}

// Default returns a config populated with sane defaults (no devices).
func Default() Config {
	return Config{
		Poll: PollConfig{TimeoutSeconds: 25, BatchSize: 10},
		Ping: PingConfig{IntervalSeconds: 30},
		HTTP: HTTPConfig{RequestTimeoutSeconds: 20, LongpollTimeoutSeconds: 40},
		Retry: RetryConfig{
			MaxAttempts:         3,
			InitialBackoffMs:    500,
			MaxBackoffMs:        10000,
			BackoffMultiplier:   2.0,
			NetworkBackoffMs:    2000,
			NetworkBackoffMaxMs: 30000,
		},
		Log: LogConfig{Level: "info"},
	}
}

// DefaultPath returns the platform default config path.
func DefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		programData := os.Getenv("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "mirai-agent", "config.toml")
	case "darwin":
		return "/Library/Application Support/mirai-agent/config.toml"
	default:
		return "/etc/mirai-agent/config.toml"
	}
}

// Load reads and validates a config file, applying defaults for zero-valued fields.
func Load(path string) (Config, error) {
	cfg := Default()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyDefaults fills any zero-valued numeric knobs with defaults so a partial
// config file still runs sanely.
func (c *Config) applyDefaults() {
	d := Default()
	if c.Poll.TimeoutSeconds == 0 {
		c.Poll.TimeoutSeconds = d.Poll.TimeoutSeconds
	}
	if c.Poll.BatchSize == 0 {
		c.Poll.BatchSize = d.Poll.BatchSize
	}
	if c.Ping.IntervalSeconds == 0 {
		c.Ping.IntervalSeconds = d.Ping.IntervalSeconds
	}
	if c.HTTP.RequestTimeoutSeconds == 0 {
		c.HTTP.RequestTimeoutSeconds = d.HTTP.RequestTimeoutSeconds
	}
	if c.HTTP.LongpollTimeoutSeconds == 0 {
		c.HTTP.LongpollTimeoutSeconds = d.HTTP.LongpollTimeoutSeconds
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = d.Retry.MaxAttempts
	}
	if c.Retry.InitialBackoffMs == 0 {
		c.Retry.InitialBackoffMs = d.Retry.InitialBackoffMs
	}
	if c.Retry.MaxBackoffMs == 0 {
		c.Retry.MaxBackoffMs = d.Retry.MaxBackoffMs
	}
	if c.Retry.BackoffMultiplier == 0 {
		c.Retry.BackoffMultiplier = d.Retry.BackoffMultiplier
	}
	if c.Retry.NetworkBackoffMs == 0 {
		c.Retry.NetworkBackoffMs = d.Retry.NetworkBackoffMs
	}
	if c.Retry.NetworkBackoffMaxMs == 0 {
		c.Retry.NetworkBackoffMaxMs = d.Retry.NetworkBackoffMaxMs
	}
	if c.Log.Level == "" {
		c.Log.Level = d.Log.Level
	}
	// Clamp poll knobs to server-accepted ranges.
	if c.Poll.BatchSize < 1 {
		c.Poll.BatchSize = 1
	}
	if c.Poll.BatchSize > 10 {
		c.Poll.BatchSize = 10
	}
	if c.Poll.TimeoutSeconds < 0 {
		c.Poll.TimeoutSeconds = 0
	}
	if c.Poll.TimeoutSeconds > 30 {
		c.Poll.TimeoutSeconds = 30
	}
}

// Validate checks that the config is coherent enough to run a worker.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.BaseURL) == "" {
		return fmt.Errorf("server.base_url is required")
	}
	if len(c.Devices) == 0 {
		return fmt.Errorf("at least one [[devices]] entry is required")
	}
	seen := map[string]bool{}
	for i := range c.Devices {
		dev := &c.Devices[i]
		if strings.TrimSpace(dev.Token) == "" {
			return fmt.Errorf("device %d (%s): token is required", dev.ID, dev.Name)
		}
		if seen[dev.Token] {
			return fmt.Errorf("duplicate device token for %q", dev.Name)
		}
		seen[dev.Token] = true
		if dev.WidthDots <= 0 {
			return fmt.Errorf("device %d (%s): width_dots must be positive", dev.ID, dev.Name)
		}
		if err := validatePrinter(dev.Printer); err != nil {
			return fmt.Errorf("device %d (%s): %w", dev.ID, dev.Name, err)
		}
	}
	return nil
}

func validatePrinter(p PrinterConfig) error {
	switch p.Kind {
	case KindWindowsSpooler:
		if p.SpoolerName == "" {
			return fmt.Errorf("windows_spooler requires spooler_name")
		}
	case KindCUPSRaw:
		if p.Queue == "" {
			return fmt.Errorf("cups_raw requires queue")
		}
	case KindDevLP:
		if p.Path == "" {
			return fmt.Errorf("dev_lp requires path")
		}
	case KindUSB:
		if p.VendorID == "" || p.ProductID == "" {
			return fmt.Errorf("usb requires vendor_id and product_id")
		}
	case "":
		return fmt.Errorf("printer.kind is required")
	default:
		return fmt.Errorf("unknown printer.kind %q", p.Kind)
	}
	return nil
}

// Save writes the config to disk with owner-only permissions (0600) and ensures
// the parent directory exists.
func Save(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	// Re-assert perms in case the file already existed with looser perms.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("chmod config: %w", err)
		}
	}
	return nil
}

// LoadRaw reads a config without requiring devices/base_url (used by setup to
// merge into an existing file). Missing file returns defaults and false.
func LoadRaw(path string) (Config, bool, error) {
	cfg := Default()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, false, nil
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, true, nil
}
