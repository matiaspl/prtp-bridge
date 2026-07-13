# PRTP Bridge

`prtp-bridge` is the config-first Kroma PRTP gateway, Web UI, audio bridge, and local matrix-helper implementation.

Run the bridge:

```bash
go run ./cmd/prtp-bridge serve -config examples/prtp-bridge.example.json --instance NET0
```

Run the local matrix helper:

```bash
go run ./cmd/prtp-matrix-helper -config examples/prtp-bridge.example.json --instance NET0
```

The bridge serves the embedded UI on `/`, accepts JSON control WebSockets on the
configured `websocket_paths.control` path, and accepts binary PCM WebSocket audio
on `websocket_paths.audio`. Defaults match the current deployed split:
`/control` and `/audio-stream`.

`udp.rx_reorder_ms` controls the optional RX sequence reorder buffer. The default
is `0`, which disables buffering and processes packets immediately while still
counting sequence gaps in the stats stream. Set it to a small positive value only
if a capture proves genuine out-of-order delivery.

The `emulation` config controls the PRTP endpoint the bridge presents to the
matrix. Set `emulation.device` to `auto` to read the matrix map and emulate the
device type assigned to the selected NET port, for example BP7100 on NET0 or
TP5024 on NET3.

On Linux, server audio is exposed as a JACK client. The default JACK client name
is derived from the selected config instance, for example
`prtp-bridge-NET3`; without an instance it falls back to the configured
`matrix_port`. Override it with `audio.client_name` only when an instance needs
a fixed custom JACK name.

Matrix operations go through the helper over a Unix-domain HTTP socket:

- `GET /healthz`
- `POST /v1/matrix/names`
- `POST /v1/matrix/crosspoint`

The helper owns TCP/2222 matrix access and serializes matrix requests. Crosspoint
mutation is intentionally gated and currently returns `501 not_implemented`
until the native single-crosspoint write frame is proven byte-for-byte.

## Deployment

The installer script at `deploy/systemd/install-prtp-bridge.sh` handles a
full install or upgrade on a Linux host running systemd.

### Pre-requisites on the target host

**Network** — the interface that carries `192.168.7.x` intercom traffic must
have a static IP configured before the bridge starts.  Using NetworkManager:

```bash
# bind the TP-Link (or equivalent) dongle to the intercom subnet
sudo nmcli connection add type ethernet con-name static-eth1 \
     ifname eth1 ip4 192.168.7.113/24
sudo nmcli connection up static-eth1
```

Substitute the correct interface name and IP for your site.

**TLS certificate** — the bridge requires a TLS cert/key pair.  Generate a
self-signed cert for testing or install a real one:

```bash
sudo mkdir -p /etc/kroma/tls
sudo openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
     -keyout /etc/kroma/tls/prtp.key \
     -out    /etc/kroma/tls/prtp.crt \
     -subj '/CN=<host-ip-or-fqdn>'
```

The installed cert and key must be readable by the service user (see below).
The installer does **not** generate or copy TLS files; manage them separately.

**G.711 tx-map (optional)** — if a per-instance gain map JSON is needed, place
it at the path referenced in `instances.<NET>.g711.tx_map` before starting the
services.  The default config ships with no tx-map paths set; the built-in
G.711 table is used when `tx_map` is empty.

**Linux JACK build** — local server audio is compiled only when CGO is enabled.
A normal `GOOS=linux GOARCH=arm64 go build` from macOS disables CGO and produces
a web/UDP-only binary whose UI reports server audio as unsupported. Build the
bridge natively on the target, or configure a Linux cross-C compiler:

```bash
# Preferred when the target has Go and a C compiler.
CGO_ENABLED=1 go build -o /tmp/prtp-bridge ./cmd/prtp-bridge

# Example cross-build; install the matching cross toolchain first.
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  go build -o /tmp/prtp-bridge ./cmd/prtp-bridge
```

On Debian, install the native JACK fallback and test tools with
`sudo apt install jackd2`. For PipeWire-JACK, also install
`sudo apt install pipewire-jack`.

### Cross-compile and install (from a Mac/Linux dev machine)

```bash
# build linux/arm64 binaries (adjust GOARCH and CC for x86-64 targets)
env CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
  go build -o /tmp/prtp-bridge ./cmd/prtp-bridge
env GOOS=linux GOARCH=arm64 go build -o /tmp/prtp-matrix-helper ./cmd/prtp-matrix-helper

# copy the deploy directory to the target
scp -r deploy/systemd <user>@<host>:/home/<user>/prtp-bridge-deploy
scp /tmp/prtp-bridge /tmp/prtp-matrix-helper <user>@<host>:/home/<user>/prtp-bridge-deploy/

# run the installer (creates service user 'prtp' by default)
ssh <user>@<host> "
  chmod +x /home/<user>/prtp-bridge-deploy/install-prtp-bridge.sh
  sudo /home/<user>/prtp-bridge-deploy/install-prtp-bridge.sh \
    --no-build \
    --bridge-bin  /home/<user>/prtp-bridge-deploy/prtp-bridge \
    --helper-bin  /home/<user>/prtp-bridge-deploy/prtp-matrix-helper \
    --config      /home/<user>/prtp-bridge-deploy/prtp-bridge.json
"
```

#### `--user` option

The installer creates a dedicated system account (`prtp` by default) and adds
it to the `audio` group.  To use a different account:

```bash
sudo ./install-prtp-bridge.sh ... --user myuser
```

If the account already exists the installer skips creation and only ensures
`audio` group membership.  After installation, make sure the TLS files are
readable by the chosen user:

```bash
sudo chown -R prtp:prtp /etc/kroma/tls
```

PipeWire normally runs as a per-user service. A bridge system unit can use it
only when it runs as that same user. Keep that user's manager alive across
logout and wrap the bridge with `pw-jack`; for example, for user `cm` (UID
1000):

```bash
sudo loginctl enable-linger cm
systemctl --user enable --now pipewire.service wireplumber.service
sudo systemctl edit prtp-bridge@.service
```

Use this drop-in, adjusting the UID if necessary:

```ini
[Service]
Environment=XDG_RUNTIME_DIR=/run/user/1000
ExecStart=
ExecStart=/usr/bin/pw-jack /opt/kroma/prtp-bridge serve -config /etc/kroma/prtp-bridge.json --instance %i
```

For native JACK, no `pw-jack` wrapper is needed. If JACK reports real-time or
memory-lock warnings, add `LimitRTPRIO=95` and `LimitMEMLOCK=infinity` to the
bridge and JACK server units. PAM limits alone do not apply to system services.

#### Installed paths

| Path | Content |
|---|---|
| `/opt/kroma/prtp-bridge` | gateway binary |
| `/opt/kroma/prtp-matrix-helper` | matrix helper binary |
| `/etc/kroma/prtp-bridge.json` | active config |
| `/etc/kroma/tls/prtp.crt` | TLS certificate (managed separately) |
| `/etc/kroma/tls/prtp.key` | TLS private key (managed separately) |
| `/etc/systemd/system/prtp-matrix-helper.service` | helper unit |
| `/etc/systemd/system/prtp-bridge@.service` | per-instance bridge unit |
| `/etc/systemd/system/prtp-bridge.target` | aggregate target |
| `/var/backups/prtp-bridge/<timestamp>/` | backup of previous install |

## Systemd Operations

Restart the whole deployed stack on the VM:

```bash
sudo systemctl restart prtp-matrix-helper.service prtp-bridge.target
```

Check the helper, target, and per-NET bridge instances:

```bash
systemctl --no-pager --plain status \
  prtp-matrix-helper.service \
  prtp-bridge.target \
  prtp-bridge@NET0.service \
  prtp-bridge@NET1.service \
  prtp-bridge@NET2.service \
  prtp-bridge@NET3.service
```

Restart only the bridge instances when the matrix helper does not need a restart:

```bash
sudo systemctl restart prtp-bridge.target
```

Before enabling server audio, confirm that the installed bridge was built with
CGO and inspect the JACK graph:

```bash
go version -m /opt/kroma/prtp-bridge | grep CGO_ENABLED
pw-jack jack_lsp -pt       # PipeWire-JACK
jack_lsp -pt               # native JACK
```

Run the integration test against an already running JACK server:

```bash
KROMA_JACK_INTEGRATION=1 go test ./internal/prtpbridge/audio -run JACK
```

For PipeWire, run both the test binary and JACK tools through `pw-jack`:

```bash
XDG_RUNTIME_DIR=/run/user/1000 pw-jack env KROMA_JACK_INTEGRATION=1 \
  go test ./internal/prtpbridge/audio -run JACK -count=1 -v
```

The current miniaudio JACK backend discovers its channel count from physical
JACK ports. A duplex test therefore needs at least one physical audio source
and sink. Some PipeWire graphs, including output-only hosts, can make
miniaudio fail with `Failed to query physical ports` even when `jack_lsp`
shows other ports. Treat the integration test as the gate; use native JACK or
attach/configure a real capture device when PipeWire fails that gate.

If no JACK server is running and a dummy server is acceptable for the test:

```bash
KROMA_JACK_INTEGRATION=1 KROMA_JACK_START_DUMMY=1 go test ./internal/prtpbridge/audio -run JACK
```

The test starts a named duplex bridge client, waits for both JACK source and
sink ports, connects the client output back to its input, then checks loopback
correlation, gain, and normalized RMS error.
