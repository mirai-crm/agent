# mirai-agent — Adding new devices

A **device** is one logical CRM printer identified by a single `secretToken`.
Each device maps to exactly one physical printer. One `mirai-agent` process
serves many devices (one `[[devices]]` block per token, one worker goroutine
each). This guide explains how to add a new device to an agent.

## Prerequisites

- The device already exists in the CRM and you have its `secretToken`
  (Bearer token). The device `type` must be `receipt_printer` — other types are
  rejected during setup.
- The CRM base URL (e.g. `https://crm.example.com`).
- The physical printer is connected to this machine and reachable through one of
  the supported backends (see [Printer kinds](#printer-kinds)).
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
`--printer` and pass `--yes`:

```bash
sudo mirai-agent setup --yes \
  --api-url https://crm.example.com \
  --token dev_live_NEW_TOKEN \
  --printer 57=cups_raw:thermal_raw
```

Add several devices at once by repeating `--token` (and `--printer`):

```bash
sudo mirai-agent setup --api-url https://crm.example.com \
  --token dev_live_A --printer 57=dev_lp:/dev/usb/lp0 \
  --token dev_live_B --printer 58=cups_raw:thermal_raw
```

What `setup` does per token:

1. `GET /api/v1/devices/info` to validate the token and read `id`/`name`/`type`.
   A `401` (invalid/archived) or a non-`receipt_printer` type → the token is
   logged and skipped.
2. Resolves the printer binding (from `--printer`, or interactively from
   discovered printers / manual entry).
3. Writes/updates the `[[devices]]` block in `config.toml` (mode `0600`).
4. Installs/starts the service unless `--no-service` was passed.

`--printer` value format is `deviceRef=printerRef`, where `deviceRef` is the
device `id` or `name`, and `printerRef` is `kind:args` (see below). Use
`--no-service` to only write config without touching the service.

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

## Verify

```bash
mirai-agent status            # lists devices, printer bindings, service state
```

During interactive `setup` you can also run a **test print** (short ESC/POS
init + "OK" + cut) to confirm the printer binding before committing.

## Removing / changing a device

- To change a device's printer, re-run `setup` with the same token and a new
  `--printer`, or edit its `[devices.printer]` block and restart the service.
- To remove a device, delete its `[[devices]]` block from `config.toml` and
  restart the service. If the token is invalid/archived server-side, the worker
  logs `401` and stops serving that token on its own (other devices keep going).

## Extending: new device or task types (developers)

Current scope is `receipt_printer` with tasks `print_check` and
`print_z_report`. To support more:

- **New task type**: add its name/const in
  [internal/api/types.go](internal/api/types.go) and a `case` in
  `execute()` in [internal/worker/worker.go](internal/worker/worker.go). Unknown
  task names are already finalized with an `error_message` instead of crashing.
- **New printer backend** (e.g. TSPL/label printers): add a `Kind*` constant and
  validation in [internal/config/config.go](internal/config/config.go), a
  backend implementing the `Printer` interface in `internal/printer/` (with the
  appropriate `//go:build` tags), and wire it into the factory in
  [internal/printer/printer.go](internal/printer/printer.go). The task dispatcher
  and print core stay unchanged.
