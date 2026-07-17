# mirai-agent

Cross-platform Go agent that prints CRM receipts and Z-reports on ESC/POS thermal
printers and executes direct-TCP PrivatBank POS terminal purchases. It polls
the CRM device API (long-poll), downloads the server-rendered PNG for print
tasks, converts it to an ESC/POS raster stream (`GS v 0`), executes purchase
tasks against bound POS terminals, and reports the result back. See
[go-mirai-agent-spec.md](go-mirai-agent-spec.md) for the full specification.

## Features

- Windows, Linux and macOS from one codebase.
- Multiple devices (tokens) per process, one goroutine each.
- Long-poll task queue, no client cursor.
- Local retry/backoff for transient errors (no server-side retries).
- Heartbeat (`/ping`) independent of task flow.
- Printer backends: Windows spooler (RAW), CUPS raw queue, `/dev/usb/lp*`, direct USB (gousb).
- Direct TCP PrivatBank POS terminal purchases over Wi-Fi/Ethernet (`host:port`, usually port `2000`).
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

# POS terminal binding (recommended for pos_terminal devices):
sudo ./mirai-agent setup --api-url https://crm.example.com --yes \
  --token dev_live_pos... --terminal 57=192.0.2.25:2000

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

For `pos_terminal` devices, use `--terminal deviceRef=host:port`. This agent
supports direct TCP only (typically the terminal's Wi-Fi/Ethernet endpoint on
port `2000`). USB/COM integrations and the genericDriverJson WebSocket bridge
are intentionally out of scope here.

## Configuration

See [config.example.toml](config.example.toml). The config file stores device
secret tokens and is written with `0600` permissions. Default paths:

- Linux: `/etc/mirai-agent/config.toml`
- Windows: `C:\ProgramData\mirai-agent\config.toml`
- macOS: `/Library/Application Support/mirai-agent/config.toml`

### POS Terminals

CRM task and result contract: [POS_TASKS.md](POS_TASKS.md).

Use `type = "pos_terminal"` and a `[devices.pos]` block for direct-TCP payment
devices:

```toml
[[devices]]
token = "dev_live_pos_TOKEN"
id = 57
name = "Front POS"
type = "pos_terminal"

  [devices.pos]
  address = "192.0.2.25:2000"
  connect_timeout_seconds = 5
  operation_timeout_seconds = 180

    [devices.pos.merchant_ids]
    "1111111111" = "1"
    "2222222222" = "3"
```

`address` must be a TCP `host:port` endpoint. `connect_timeout_seconds` defaults
to `5` and `operation_timeout_seconds` defaults to `180`. The terminal's
factory/default port is usually `2000`. A single NUL-delimited terminal frame
is limited to 1 MiB; oversized frames are rejected and the TCP session is
discarded.

Purchase tasks arrive as CRM task `purchase` with input like
`{"amountMinor":12345,"tin":"1111111111"}` where `amountMinor` is in kopecks
(`12345` = `123.45`). The agent resolves the terminal `merchantId` through
`devices.pos.merchant_ids`; an empty or unbound `tin` is rejected without
contacting the terminal.

Successful finalization data keeps the original top-level `amountMinor` and
`tin`, then adds a `payment` object with:

- `status`: `approved`, `partial`, `declined`, or `unknown`
- `requestSent`: whether the Purchase request reached the terminal socket
- `stage`: the last transport stage (`completed`, `await_response`, etc.), or
  `recovered_after_restart` when an incomplete journal intent is recovered
- `response`: the full terminal response envelope after sanitizing
  `track1`, `cardHolderName`, and `cardExpiryDate`

When the terminal never yields a durable response, the agent finalizes with
`payment.status = "unknown"` and reuses the saved input. Its `payment.stage` is
either the saved last transport stage or the synthesized
`recovered_after_restart` stage for an incomplete journal intent. Operators must
never automatically retry an `unknown`; reconcile that payment first. For
`partial`, treat the payment by the actually approved amount reported by the
terminal response, not by the original request amount.

The agent stores unresolved POS state in a local journal at
`<configPath>.payments.json` with mode `0600`. On restart it replays the saved
finalize payload to the CRM and must not send a second Purchase for the same
task. Do not delete that journal while unresolved entries still exist.

Refunds, withdrawals/cancellation, USB/COM terminal access, and the
genericDriverJson WebSocket path are out of scope for this agent.

## Exit codes

`0` ok, `1` general, `2` usage, `3` config, `4` bootstrap (no valid tokens),
`5` service permissions, `6` printer self-check.
