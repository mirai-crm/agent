# mirai-agent — Adding new devices

A **device** is one logical CRM endpoint identified by a single `secretToken`.
Each device maps to exactly one physical printer or one physical POS terminal.
One `mirai-agent` process serves many devices (one `[[devices]]` block per
token, one worker goroutine each). This guide explains how to add a new device
to an agent.

## Prerequisites

- The device already exists in the CRM and you have its `secretToken`
 (Bearer token). The device `type` must be `receipt_printer`, `label_printer`,
 or `pos_terminal` — other types are rejected during setup.
- The CRM base URL (e.g. `https://crm.example.com`).
- For `receipt_printer`, the physical printer is connected to this machine and
 reachable through one of the supported backends (see
 [Printer kinds](#printer-kinds)).
- For `label_printer`, the physical TSPL printer is reachable through a raw
 printer backend. Direct USB uses the existing libusb backend.
- For `pos_terminal`, the physical terminal is reachable over direct TCP
 (`host:port`) on the local network. The default terminal port is usually
 `2000`.
- Admin/root rights if the agent runs as a service (installing/restarting it).

## Method A — `mirai-agent setup` (recommended)

`setup` is idempotent: re-running it adds new tokens and updates existing ones
in place (matched by token) without disturbing other devices. On the first run
it also persists the API host into `server.base_url`; afterwards `--api-url` is
optional.

Interactive (prompts for the printer binding and print width, offers a test
print):

```bash
sudo mirai-agent setup \
  --api-url https://crm.example.com \
  --token dev_live_NEW_TOKEN
```

Non-interactive (CI/automation) — you must provide the printer binding via
`--printer` / `--terminal` and pass `--yes`:

```bash
sudo mirai-agent setup --yes \
  --api-url https://crm.example.com \
  --token dev_live_NEW_TOKEN \
  --printer 57=cups_raw:thermal_raw

sudo mirai-agent setup --yes \
 --api-url https://crm.example.com \
 --token dev_live_POS_TOKEN \
 --terminal 81=192.0.2.25:2000
```

Add several devices at once by repeating `--token` (and `--printer`):

```bash
sudo mirai-agent setup --api-url https://crm.example.com \
  --token dev_live_A --printer 57=dev_lp:/dev/usb/lp0 \
  --token dev_live_B --printer 58=cups_raw:thermal_raw \
  --token dev_live_POS --terminal 81=192.0.2.25:2000
```

What `setup` does per token:

1. `GET /api/v1/devices/info` to validate the token and read `id`/`name`/`type`.
 A `401` (invalid/archived) or a type outside
 `receipt_printer|label_printer|pos_terminal` → the token is logged and skipped.
2. Resolves the binding:
 - `receipt_printer`: from `--printer`, or interactively from discovered
   printers / manual entry.
 - `label_printer`: from the same `--printer` binding; setup emits TSPL for its
   optional test print.
 - `pos_terminal`: from `--terminal deviceRef=host:port`, or interactively by
   entering the TCP address.
3. Writes/updates the `[[devices]]` block in `config.toml` (mode `0600`).
4. Installs/starts the service unless `--no-service` was passed.

`--printer` value format is `deviceRef=printerRef`, where `deviceRef` is the
device `id` or `name`, and `printerRef` is `kind:args` (see below). Use
`--no-service` to only write config without touching the service.
For `pos_terminal`, `--terminal` uses `deviceRef=host:port`.

If **no** token passes validation, `setup` exits with code `4` and does not
install the service.

## Method B — edit `config.toml` manually

Use this when scripting config generation or when the CRM is unreachable at the
time of provisioning. Add a `[[devices]]` block and restart the service.

Config paths:

- Linux: `/etc/mirai-agent/config.toml`
- Windows: `C:\ProgramData\mirai-agent\config.toml`
- macOS: `/Library/Application Support/mirai-agent/config.toml`

Example block (one per device):

```toml
[[devices]]
token = "dev_live_NEW_TOKEN"   # secret Bearer token — keep the file at 0600
id = 57                         # from GET /info (used for readable logs)
name = "Point #3"
width_dots = 576                # 58mm ~= 384, 80mm ~= 576
png_scale = 0                   # 0 => do not send ?scale

  [devices.printer]
  kind = "cups_raw"             # windows_spooler | cups_raw | dev_lp | usb
  queue = "thermal_raw"
```

POS terminal example:

```toml
[[devices]]
token = "dev_live_POS_TOKEN"     # secret Bearer token — keep the file at 0600
id = 81                          # from GET /info (used for readable logs)
name = "Cash desk POS"
type = "pos_terminal"

  [devices.pos]
  address = "192.0.2.25:2000"    # direct TCP over Wi-Fi/Ethernet
  connect_timeout_seconds = 5
  operation_timeout_seconds = 180

    [devices.pos.merchant_ids]    # task tin -> terminal merchantId
    "1111111111" = "1"
    "2222222222" = "3"
```

TSPL label printer example:

```toml
[[devices]]
token = "dev_live_LABEL_TOKEN"
id = 82
name = "Warehouse labels"
type = "label_printer"

  [devices.printer]
  kind = "usb"
  vendor_id = "0x1234"
  product_id = "0x5678"

  [devices.label]
  dpi = 203
  gap_mm = 2
  gap_offset_mm = 0
```

Then apply the change:

```bash
# Linux (systemd)
sudo systemctl restart MiraiAgent      # or: sudo mirai-agent uninstall && sudo mirai-agent install
# macOS (launchd)
sudo launchctl kickstart -k system/MiraiAgent
# Windows (admin)
sc stop MiraiAgent && sc start MiraiAgent
```

Keep the config readable only by the service account (`chmod 600` on
Linux/macOS; `setup`/`install` sets this automatically).

## Printer kinds

Set exactly one `[devices.printer]` block. The `--printer` `printerRef` uses the
`kind:args` forms in the last column.

| kind | Platform | Config fields | `printerRef` example |
| --- | --- | --- | --- |
| `windows_spooler` | Windows | `spooler_name` | `windows_spooler:XP-58 (RAW)` |
| `cups_raw` | Linux/macOS | `queue` | `cups_raw:thermal_raw` |
| `dev_lp` | Linux | `path` | `dev_lp:/dev/usb/lp0` |
| `usb` | all (cgo build) | `vendor_id`, `product_id`, `serial?` | `usb:0x0416:0x5011` or `usb:0x0416:0x5011:SERIAL` |

Notes:

- `cups_raw` requires a **raw** CUPS queue:
  `lpadmin -p thermal_raw -E -v <device-uri> -m raw`.
- `dev_lp`: the service user must have access to the character device (group
  `lp`, or a udev rule).
- `usb` requires a `CGO_ENABLED=1` build with libusb installed, and USB access
  rights (udev rule or root); on Linux the kernel `usblp` driver is auto-detached.

## POS terminals

`pos_terminal` devices use direct TCP only (`host:port`) and do not use a
printer backend. The recommended setup path is:

```bash
sudo mirai-agent setup --api-url https://crm.example.com \
  --token dev_live_POS_TOKEN \
  --terminal 81=192.0.2.25:2000
```

Notes:

- The terminal's default TCP port is usually `2000`.
- Only direct Wi-Fi/Ethernet TCP is supported here.
- USB/COM integrations and the genericDriverJson WebSocket bridge are out of
  scope.

Purchase tasks arrive as CRM task `purchase` with input like
`{"amountMinor":12345,"tin":"1111111111"}` where `amountMinor` is in kopecks.
The agent resolves the terminal `merchantId` from `devices.pos.merchant_ids`;
an empty or unbound `tin` is rejected before contacting the terminal.

Finalize payloads keep the top-level `amountMinor` and `tin`, then add a
`payment` object with `status`, `requestSent`, `stage`, and the full sanitized
terminal `response`. Sensitive response fields `track1`, `cardHolderName`, and
`cardExpiryDate` are always removed before finalization. `status` is one of
`approved`, `partial`, `declined`, or `unknown`.

Operational rules:

- Never automatically retry an `unknown`; reconcile it first.
- For `partial`, treat the payment by the actually approved amount reported by
  the terminal response.
- The local journal lives at `<configPath>.payments.json` with mode `0600`.
  Replays after restart finalize the saved result without sending a second
  Purchase. Do not delete the journal while unresolved entries still exist.
- Refunds, withdrawals, and cancellation flows are out of scope.

## Verify

```bash
mirai-agent status            # lists devices, printer bindings, service state
```

During interactive `setup` you can also run a protocol-appropriate **test
print** (ESC/POS receipt or TSPL label) before committing. POS terminal bindings
are validated by TCP address parsing; there is no interactive purchase smoke
test.

## Removing / changing a device

- To change a device's printer, re-run `setup` with the same token and a new
 `--printer`, or edit its `[devices.printer]` block and restart the service.
- To change a POS terminal binding, re-run `setup` with the same token and a new
 `--terminal`, or edit its `[devices.pos]` block and restart the service.
- To remove a device, delete its `[[devices]]` block from `config.toml` and
  restart the service. If the token is invalid/archived server-side, the worker
  logs `401` and stops serving that token on its own (other devices keep going).

## Extending: new device or task types (developers)

Current scope is:

- `receipt_printer` with tasks `print_check` and `print_z_report`
- `label_printer` with task `print_label`
- `pos_terminal` with task `purchase`

To support more:

- **New task type**: add its name/const in
  [internal/api/types.go](internal/api/types.go) and a `case` in
  `execute()` in [internal/worker/worker.go](internal/worker/worker.go). Unknown
  task names are already finalized with an `error_message` instead of crashing.
- **New transport backend**: add a `Kind*` constant and validation in
  [internal/config/config.go](internal/config/config.go), implement the raw
  `Printer` interface in `internal/printer/` with appropriate build tags, and
  wire it into [internal/printer/printer.go](internal/printer/printer.go).
  Command languages such as ESC/POS and TSPL belong in renderer packages and
  reuse these transports.
