# mirai-agent

Cross-platform Go agent that prints CRM receipts and Z-reports on ESC/POS thermal
printers. It polls the CRM device API (long-poll), downloads the server-rendered
PNG, converts it to an ESC/POS raster stream (`GS v 0`), prints it, and reports
the result back. See [go-mirai-agent-spec.md](go-mirai-agent-spec.md) for the
full specification.

## Features

- Windows, Linux and macOS from one codebase.
- Multiple devices (tokens) per process, one goroutine each.
- Long-poll task queue, no client cursor.
- Local retry/backoff for transient errors (no server-side retries).
- Heartbeat (`/ping`) independent of task flow.
- Printer backends: Windows spooler (RAW), CUPS raw queue, `/dev/usb/lp*`, direct USB (gousb).
- Installs as a service (systemd / Windows SCM / launchd) via `kardianos/service`.

## Requirements

- Go 1.23+.
- Direct USB printing (`kind = "usb"`) requires a cgo build with **libusb**:
  - macOS: `brew install libusb`
  - Debian/Ubuntu: `apt install libusb-1.0-0-dev`
  - Build with `CGO_ENABLED=1`. Other backends build fine with `CGO_ENABLED=0`.

## Build

```bash
# Native (enables the USB backend if libusb is present):
CGO_ENABLED=1 go build -o mirai-agent ./cmd/agent

# Without USB (no cgo/libusb needed):
CGO_ENABLED=0 go build -o mirai-agent ./cmd/agent
```

## Usage

```bash
# First run: discover devices by token, bind printers, write config, install service.
sudo ./mirai-agent setup \
  --api-url https://crm.example.com \
  --token dev_live_a1b2... --token dev_live_9z8y...

# Non-interactive binding:
sudo ./mirai-agent setup --api-url https://crm.example.com --yes \
  --token dev_live_a1b2... --printer 42=cups_raw:thermal_raw

# Run in foreground (usually started by the service):
./mirai-agent run --config /etc/mirai-agent/config.toml

# Service management and status:
sudo ./mirai-agent install
sudo ./mirai-agent uninstall
./mirai-agent status
```

Printer refs for `--printer deviceRef=printerRef` (deviceRef = device id or name):

| kind | example ref |
| --- | --- |
| `dev_lp` | `dev_lp:/dev/usb/lp0` |
| `cups_raw` | `cups_raw:thermal_raw` |
| `windows_spooler` | `windows_spooler:XP-58 (RAW)` |
| `usb` | `usb:0x0416:0x5011` or `usb:0x0416:0x5011:SERIAL` |

## Configuration

See [config.example.toml](config.example.toml). The config file stores device
secret tokens and is written with `0600` permissions. Default paths:

- Linux: `/etc/mirai-agent/config.toml`
- Windows: `C:\ProgramData\mirai-agent\config.toml`
- macOS: `/Library/Application Support/mirai-agent/config.toml`

## Exit codes

`0` ok, `1` general, `2` usage, `3` config, `4` bootstrap (no valid tokens),
`5` service permissions, `6` printer self-check.
