# Repository Guidelines

This repository contains the standalone `prtp-bridge` runtime for legacy Kroma 
intercom systems integration and its local matrix helper.

## Project Structure

- `cmd/prtp-bridge/` - gateway, Web UI, and audio bridge process.
- `cmd/prtp-matrix-helper/` - local Unix-socket HTTP helper that owns matrix
  TCP/2222 access.
- `internal/prtpbridge/` - bridge packages for config, PRTP framing, G.711,
  audio, gateway runtime, matrix parsing, helper API, and embedded Web UI.
- `internal/g711/` - built-in Kroma G.711 decode table.
- `deploy/systemd/` - VM/systemd installer, units, and example deployed config.
- `examples/` - local config examples.
- `docs/` - bridge and protocol notes.
- `scripts/` - operational analysis/configuration helpers.

## Build And Test

```bash
go build ./cmd/...
go test ./...
```

Use `make build` and `make test` for the same common checks.

JACK integration is opt-in and should run on the Kroma VM or another host with
JACK tooling:

```bash
KROMA_JACK_INTEGRATION=1 KROMA_JACK_START_DUMMY=1 go test ./internal/prtpbridge/audio -run JACK -count=1 -v
```

## Coding Rules

- Keep the app config-first and JSON-only unless a dependency decision is made
  explicitly.
- Keep native matrix crosspoint writes behind the documented discovery gate.
  Do not use `/CHANGEXPT`, `/XPTS`, `/SAVEXPT`, or the old matrix HTTP client.
- Preserve the helper boundary: matrix TCP/2222 access belongs in
  `prtp-matrix-helper`.
- Use `gofmt` for all Go files.
- Add focused tests for protocol framing, matrix parsing, audio flow, and WebSocket
  behavior when changing those areas.
