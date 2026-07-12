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

## Download

Prebuilt binaries are published on the
[GitHub Releases](https://github.com/mirai-crm/agent/releases) page. Every
release is a native cgo build with the USB backend enabled:

| Platform | Asset |
| --- | --- |
| Linux x86_64 | `mirai-agent_<version>_linux_amd64.tar.gz` |
| Linux arm64 | `mirai-agent_<version>_linux_arm64.tar.gz` |
| macOS Intel | `mirai-agent_<version>_darwin_amd64.tar.gz` |
| macOS Apple Silicon | `mirai-agent_<version>_darwin_arm64.tar.gz` |
| Windows x86_64 | `mirai-agent_<version>_windows_amd64.zip` |

Verify a download against `checksums.txt` (`sha256sum -c` /
`shasum -a 256 -c`). Notes:

- **Linux/macOS** need the libusb runtime for `kind = "usb"`
  (`apt install libusb-1.0-0` / `brew install libusb`). Other backends work
  without it.
- **Windows** archives ship `libusb-1.0.dll` next to `mirai-agent.exe`; keep
  them together.
- macOS binaries are **not** signed or notarized yet; clear the quarantine
  attribute (`xattr -d com.apple.quarantine mirai-agent`) or allow it in
  System Settings on first run.

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

## Releasing

Releases are built and published automatically by
[`.github/workflows/release.yml`](.github/workflows/release.yml) when a
semver tag is pushed:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The workflow runs the tests, builds all five native cgo binaries, and creates
a GitHub Release with the archives plus `checksums.txt`. The tag (without the
`v`) is embedded as the binary version (`mirai-agent --version`).

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
