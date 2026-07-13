# Native Crosspoint Discovery Gate

Status: not implemented.

The new `prtp-matrix-helper` must not use the matrix HTTP crosspoint endpoints.
Its `POST /v1/matrix/crosspoint` endpoint returns `501 not_implemented` until a
native TCP/2222 single-crosspoint command is proven with byte-level evidence.

## Confirmed TCP/2222 Read Path

The current evidence supports these replay-safe read-only vectors:

| Purpose | Payload | Wire Frame |
| --- | --- | --- |
| ACK valid info frame | `00 41` | `00 41 4B FF` |
| Matrix identity request | `00 49 0E 01 00` | `00 49 0E 01 00 E9 FF` |
| Current map bank request | `00 49 00` | `00 49 00 6B FF` |
| Download current map bank 3 | `00 49 03 03` | `00 49 03 03 0D FF` |

Map readout behavior remains:

1. Send `00 49 00`.
2. Receive `00 49 01 <current-bank> <bank-name-fields...>`.
3. Send `00 49 03 <current-bank>`.
4. Receive `00 49 04 <size-be32>`, `00 49 05 <seq> <chunk...>`, and `00 49 06`.
5. ACK every valid `00 49 ...` frame immediately with `00 41`.

## Missing Evidence

The bridge still needs a byte-exact single-crosspoint native command before
write support can be enabled:

- request frame and response frame
- readback frame proving the resulting state
- whether the command sets, clears, or toggles
- whether a persistence frame exists and how it relates to `save: true`
- ACK timing and retry behavior during mutation

Starting artifacts for that work are the `net0-net3 loopback set to TP50xx.pcapng`
capture and CrossMapper command-handler notes around the `cmd31`/XPT paths.
Until the evidence is documented here, the helper must keep returning
`501 not_implemented`.
