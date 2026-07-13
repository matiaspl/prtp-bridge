
============================================================
KROMA CROSSMAPPER TCP PROTOCOL (PORT 2222) - WIRE FORMAT
============================================================
Confirmed by static analysis of CrossMapper.exe

## Transport
- TCP port 2222 (ComType=TCP)
- Alternative: Serial COM port (ComType=Serial), same framing
- Alternative: ISDN (ComType=ISDN), same framing

## Wire Format (Outgoing Frame)

  <PAYLOAD BYTES> + <CRC-8> + <0xFF TERMINATOR>
  
  Byte-stuffing applied to bytes 1..N-1 (NOT the first byte):
    0xFE → 0xFE 0xFE
    0xFF → 0xFE 0xFF
  
  CRC-8 polynomial: 0x8D (same as PRTP)
  CRC is calculated over the payload BEFORE stuffing
  CRC is included in byte-stuffing
  0xFF terminator is NOT part of CRC calculation

## Payload Structure

  Offset  Size  Field          Value
  0       1     Header         0x00 (constant)
  1       1     Type           Frame type character
                               'A' (0x41) = ACK
                               'N' (0x4E) = NACK  
                               'P' (0x50) = PING
                               'I' (0x49) = Info (command/data)
  2       1     Subtype/len    For 'I' frames: length or sub-command
                               For 'A'/'N'/'P': absent (3-byte frame)
  3+      N     Payload data   Only for 'I' frames
  last    1     CRC-8          Over all preceding bytes

## Frame Types Confirmed

### ACK frame (3 bytes payload + CRC)
  Builder: 0x404CA0
  Wire bytes: [00][41][CRC][FF]
  Sent when a valid frame was received

### NACK frame (3 bytes payload + CRC)
  Builder: 0x404CE0
  Wire bytes: [00][4E][CRC][FF]
  Sent when a frame failed validation (bad CRC, etc.)

### PING frame (3 bytes payload + CRC)
  Builder: 0x404E10
  Wire bytes: [00][50][CRC][FF]
  Keepalive or connection test

### INFO frame (N bytes payload + CRC)
  Builders: 20+ locations in CExternalMatrix methods
  Wire bytes: [00][49][LEN][payload...][CRC][FF]
  Carries commands and data

## Send Path (in CrossMapper)

  1. Caller fills buffer with: [00, TYPE, LEN, data..., al]
  2. Calls CRC helper at 0x404980:
     - Computes CRC-8 (poly 0x8D) over buffer[0..length-1]
     - Appends CRC at buffer[length]
     - Returns length+1
  3. Calls send at 0x404A30 with (buffer, length_with_crc):
     - Copies buffer[0] directly (no escape)
     - For buffer[1..length-1]:
       * If byte == 0xFE: output 0xFE, 0xFE
       * If byte == 0xFF: output 0xFE, 0xFF
       * Else: output byte
     - Appends 0xFF terminator
  4. Based on ComType flag at 0x006797AD:
     - If cl==1 (TCP): calls CAsyncSocket::Send via vtable
     - Else (Serial): calls 0x43E920 byte-by-byte write
     - Or ISDN path

## Receive Path (in CrossMapper)

  Frame parser at 0x405120 (CExternalMatrix::OnReceive or similar)
  The parser sits in its own thread, reads from socket,
  undoes byte-stuffing (removes 0xFE prefixes, watches for 0xFF),
  validates CRC, then dispatches on byte [1]:
  
  - 'A' (0x41) at 0x4057F7: ACK handler - confirms sent frame
  - 'I' (0x49) at 0x405682: Info handler - process received data
  - 'N' (0x4E) at 0x4055FA: NACK handler - retransmit sent frame

## Info Frame Subtype Dispatch

The Info handler reads byte [10] of the RECEIVED buffer (which includes
CExternalMatrix internal metadata before the wire bytes):

  at 0x4056AA:
    0f b6 46 0a     movzx eax, byte [esi+0xA]   ; read subtype byte
    83 e8 04        sub eax, 4                   ; compare to 4
    0f 84 ...       je handler_for_4
    48              dec eax                      ; compare to 5
    74 3d           je handler_for_5
    ...

Note: The "esi+6=length, esi+8=data, esi+9=type, esi+10=subtype" 
pattern refers to an INTERNAL frame object (with metadata),
NOT the raw wire format. The wire format only has the payload + CRC + FF.

## NO AUTHENTICATION

Confirmed: TCP port 2222 has NO login, NO authentication, NO session setup.
After TCP connect(), the client can immediately send frames.
Any network client can connect and control the matrix.

## Retransmission

On NACK receipt, CrossMapper reads the stored pending frame:
  - esi+6: 16-bit length (in stored struct)
  - esi+8: payload pointer
  - Re-sends via 0x404A30

Retry counter at [0x006797B2] (increments on each retry)

TCP/2222 accept path in `USER.U8.01.02.0021.hex` allocates a slot before the
application frame exchange and does not arm the same timer in the SYN accept
path. Abandoned TCP/2222 connects can therefore clog all five shared slots and
make both HTTP and TCP/2222 stop accepting new connections.

Client rules for TCP/2222:

- Treat TCP/2222 as a scarce management session, not as a cheap poll target.
- Keep at most one TCP/2222 connection per matrix owner/helper and serialize
  CrossMapper protocol requests over it.
- Do not run parallel connection attempts or fast retry loops against the same
  matrix. Use a bounded connect timeout, then back off before retrying.
- Do not open TCP/2222 merely to test liveness. Prefer ICMP/ARP for reachability
  checks and only open TCP/2222 when sending real protocol frames.
- Once connected, send a valid request promptly, ACK every valid `00 49 ...`
  response frame, drain the expected response sequence, and close the socket
  cleanly when finished.
- On protocol timeout, parse error, or local cancellation, close the socket and
  wait before retrying. Avoid leaving idle or forgotten TCP/2222 sockets open.

The firmware-side fix is to arm the TCP/2222 per-slot timeout immediately after
accept, mirroring the HTTP path's timer setup.

## Read Current Map

CrossMapper's "Open Map from Matrix" flow is read-only and does not send the
save/commit family. Static analysis of `CrossMapper.exe` shows this sequence:

1. Send `00 49 00` to request the map-bank table.
2. Expect `00 49 01 <current-bank> <bank1[8]> <bank2[8]> ...`.
   `current-bank` is 1-based. Each bank name is an 8-byte NUL-padded field.
3. Send `00 49 03 <current-bank>` to download that bank.
4. Receive:
   - `00 49 04 <size-be32>`: map byte length.
   - `00 49 05 <seq> <chunk...>`: sequenced file chunks, starting at sequence `0`.
   - `00 49 06`: end of transfer.
5. ACK every valid `00 49 ...` frame with `00 41`.

The downloaded bytes are the `.kmp` map file. Live captures from a TH5012 matrix
show a `KMP 0x02` blob using TLV-style records. Confirmed string fields use
`0x82 <len> <ascii...>`. Observed structural/numeric tags include `0xC2`,
`0xC3`, `0xC6`, `0xCC`, `0x83`, `0x90`, and `0x91`; these still need a full
parser for route actions, but button-label grouping is now understood:

- `0xC2 <len-le16> ...`: top-level port/terminal section.
  - `0x20 <port-index>` identifies the matrix port (`DIG0` = 0, `NET3` = 11).
  - `0x21 <type>` is the device/port type selector.
  - first `0x82` string is the port/terminal name.
- `0xCC <len-le16> ...`: button/key section inside the current `0xC2` record.
  - `0x20 <key-index>` identifies the button index.
  - first `0x82` string is the button label.
  - nested `0xC3` records describe route/action targets and remain partially
    decoded.

Clients must keep labels keyed by `(port-index, key-index)`. Do not build a
button list by taking all printable strings after a port name; names like
`NET3` can also appear as button labels in earlier port sections, which shifts
labels across ports when empty buttons are omitted.

The same `0xC2` records carry the port display names, so the normal client
readout can use the downloaded map as the single source for both port names and
button labels. The `00 49 0E 00 <port>` status/name request is still useful as a
manual probe, but it is redundant for the map-backed button-name readout and
adds one matrix-paced round trip per port.

Older notes mention these records, but they were not present in the live `KMP
0x02` map download captured on 2026-04-28 and should be treated as legacy or
unconfirmed for this readout path:

- `0xAD <port> <type> <name\0> <description\0> ...`: sets the current port.
- `0xAC <key> <label\0>`: sets a key label for the current port.

## Map Readout Timing Notes

CrossMapper does not appear to insert a deliberate delay in the map-download
loop. It receives frames through its normal socket/parser path, ACKs every valid
`00 49 ...` frame with `00 41`, and writes chunk payloads as they arrive. This
means map-download throughput is sensitive to ACK turnaround time if the matrix
only sends the next chunk after seeing the ACK for the previous frame.

Live timing from a TH5012 matrix on 2026-04-28:

- Current bank: `3`, name `"Aktualna"`.
- Advertised map size: `2102` bytes.
- Transfer: `22` chunks, mostly `96` payload bytes per `00 49 05` frame.
- Elapsed map download: `11.703s`, about `180 B/s`.
- Per-chunk `frame_read`: typically `507-511 ms`.
- Total ACK write time: below `1 ms`; max ACK write about `506 us`.

This points to matrix-side pacing or a serial-compatible slow download mode, not
local TCP write delay. Port-name/status polls showed similar roughly
half-second response pacing per requested port, which is why the automatic
button-name readout now avoids that scan and takes port names directly from the
map.

For a compatible client:

- Send `00 41` immediately after every valid `00 49 ...` map frame, including
  size (`0x04`), chunk (`0x05`), and end (`0x06`) frames.
- Keep TCP small-write latency low. The Go `prtp` client explicitly enables
  `TCP_NODELAY`, uses a larger TCP reader, and reuses a prebuilt ACK frame.
- Do not reuse a short per-frame protocol timeout as the TCP connection timeout.
  CrossMapper tolerates connection setup separately; the Go `prtp` client uses
  a longer connect timeout, then shorter synchronous exchange deadlines once the
  socket is open.
- Avoid verbose packet logging during timing comparisons. `--debug-matrix`
  logs frame summaries, payload hex, per-chunk progress, and timing, which is
  useful for diagnosis but can skew throughput.

The Go `prtp` client logs map completion as:

```text
matrix tcp map download complete ... elapsed=<total> rate=<bytes/sec> frame_read=<total> ack_write=<total> max_frame_read=<max> max_ack_write=<max>
```

Interpretation:

- `ack_write` or `max_ack_write` spikes point to local TCP write/backpressure
  problems.
- `frame_read` close to `elapsed` means most time is spent waiting for or
  parsing incoming frames. That can be matrix-side pacing, network latency, or
  local frame/CRC parsing overhead.
- With `--debug-matrix` enabled, per-frame timing logs can show whether one
  specific chunk stalls or whether all chunks are evenly paced.
