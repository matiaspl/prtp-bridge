#!/usr/bin/env python3
"""Analyze PRTP control frames inside gateway UDP captures.

This expects tcpdump/libpcap files captured on the bridge host, for example:

  sudo tcpdump -i any -nn -s 0 -w /tmp/prtp-net3.pcap \
    'udp and host 192.168.7.203 and port 8087'
  python3 scripts/analyze_prtp_pcap.py /tmp/prtp-net3.pcap

The parser currently supports Linux cooked v2 captures, which is what
`tcpdump -i any` writes on the Kroma VM.
"""

import collections
import socket
import struct
import sys


KNOWN_TYPES = {0x41, 0x43, 0x49, 0x4E, 0x50, 0x52, 0x53}


def crc_residue(frame: bytes) -> int:
    if not frame:
        return 0
    res = frame[0]
    for i in range(len(frame) - 1):
        bit = 0
        while bit < 8:
            if (res & 0x80) == 0:
                next_bit = (frame[i + 1] >> (7 - bit)) & 1
                res = ((res << 1) & 0xFF) | next_bit
                bit += 1
            res ^= 0x8D
            res &= 0xFF
    return res


def calc_crc(payload: bytes) -> int:
    return crc_residue(payload + b"\x00") if payload else 0


def normalize_payload(payload: bytes) -> tuple[bytes, int | None]:
    if len(payload) >= 2 and payload[0] not in KNOWN_TYPES and payload[1] in KNOWN_TYPES:
        return payload[1:], payload[0]
    return payload, None


def frame_name(payload: bytes) -> str:
    payload, _ = normalize_payload(payload)
    if not payload:
        return "empty"
    t0 = payload[0]
    if t0 == 0x49 and len(payload) >= 2:
        sub = payload[1] & 0xF0
        return {
            0x00: "key",
            0x40: "keys",
            0x50: "g_fix",
            0x60: "r_fix",
            0x70: "g_blink",
            0x80: "r_blink",
            0x90: "label",
            0xD0: "ident",
        }.get(sub, f"I_{payload[1]:02x}")
    return {
        0x41: "ack",
        0x43: "cmd",
        0x4E: "nack",
        0x50: "ping",
        0x52: "R",
        0x53: "sync",
    }.get(t0, f"type_{t0:02x}")


def read_prtp_packets(path: str, matrix_host: str) -> list[tuple[float, str, bytes]]:
    packets: list[tuple[float, str, bytes]] = []
    with open(path, "rb") as f:
        global_header = f.read(24)
        if len(global_header) < 24:
            return packets
        magic = global_header[:4]
        endian = "<" if magic in (b"\xd4\xc3\xb2\xa1", b"\x4d\x3c\xb2\xa1") else ">"
        network = struct.unpack(endian + "I", global_header[20:24])[0]
        while True:
            packet_header = f.read(16)
            if len(packet_header) < 16:
                break
            ts, us, included_len, _ = struct.unpack(endian + "IIII", packet_header)
            data = f.read(included_len)
            if len(data) < included_len:
                continue
            if network == 276:
                if len(data) < 68:
                    continue
                ip = data[20:]  # Linux cooked v2 header is 20 bytes.
            elif network == 1:
                if len(data) < 14:
                    continue
                ether_type = struct.unpack("!H", data[12:14])[0]
                offset = 14
                while ether_type in (0x8100, 0x88A8) and len(data) >= offset + 4:
                    ether_type = struct.unpack("!H", data[offset + 2 : offset + 4])[0]
                    offset += 4
                if ether_type != 0x0800 or len(data) < offset + 48:
                    continue
                ip = data[offset:]
            else:
                raise ValueError(f"unsupported pcap link type {network}")
            if ip[0] >> 4 != 4 or ip[9] != 17:
                continue
            ihl = (ip[0] & 0x0F) * 4
            src = socket.inet_ntoa(ip[12:16])
            udp = ip[ihl : ihl + 8]
            _, _, udp_len, _ = struct.unpack("!HHHH", udp)
            payload = ip[ihl + 8 : ihl + udp_len]
            if len(payload) != 268 or payload[0] != 0xAA:
                continue
            direction = "in" if src == matrix_host else "out"
            packets.append((ts + us / 1_000_000, direction, payload))
    return packets


def classify_seq_gaps(
    direction_packets: list[tuple[float, bytes]], gaps: list[tuple[int, float, int, int, int]]
) -> tuple[list[tuple[float, int, int, int]], list[tuple[float, int, int, int, list[int]]]]:
    missing: list[tuple[float, int, int, int]] = []
    out_of_order: list[tuple[float, int, int, int, list[int]]] = []
    for idx, t, prev, cur, delta in gaps:
        late: list[int] = []
        want = [((prev + step) & 0xFF) for step in range(1, delta)]
        for late_t, packet in direction_packets[idx + 1 :]:
            if late_t - t > 2.0:
                break
            seq = packet[1]
            if seq in want and seq not in late:
                late.append(seq)
        if late:
            out_of_order.append((t, prev, cur, delta, late))
        else:
            missing.append((t, prev, cur, delta))
    return missing, out_of_order


def analyze_direction(packets: list[tuple[float, str, bytes]], direction: str) -> None:
    seq_prev: int | None = None
    gaps: list[tuple[int, float, int, int, int]] = []
    frames: list[tuple[float, bytes, bytes, str, bool, int, int]] = []
    chunks: list[tuple[float, int, bytes]] = []
    direction_packets: list[tuple[float, bytes]] = []
    acc = bytearray()
    esc = False
    packet_count = 0

    for t, packet_direction, packet in packets:
        if packet_direction != direction:
            continue
        direction_packets.append((t, packet))
        seq = packet[1]
        packet_count += 1
        if seq_prev is not None and ((seq_prev + 1) & 0xFF) != seq:
            delta = (seq - seq_prev) & 0xFF
            if 1 < delta < 128:
                gaps.append((len(direction_packets) - 1, t, seq_prev, seq, delta))
        seq_prev = seq

        if not (packet[5] & 0x04):
            continue
        nctrl = min(packet[7], 4)
        ctrl = packet[8 : 8 + nctrl]
        chunks.append((t, seq, ctrl))
        for b in ctrl:
            if esc:
                acc.append(b)
                esc = False
                continue
            if b == 0xFE:
                esc = True
                continue
            if b == 0xFF:
                if len(acc) >= 2:
                    frame = bytes(acc)
                    payload = frame[:-1]
                    ok = crc_residue(frame) == 0
                    frames.append((t, frame, payload, frame_name(payload), ok, calc_crc(payload), frame[-1]))
                acc.clear()
                continue
            acc.append(b)

    bad = [frame for frame in frames if not frame[4]]
    ok_counts = collections.Counter(name for _, _, _, name, ok, _, _ in frames if ok)
    missing, out_of_order = classify_seq_gaps(direction_packets, gaps)
    print(f"\nDIR {direction}")
    print(
        f"packets={packet_count} ctrl_packets={len(chunks)} seq_gaps={len(gaps)} "
        f"missing_gaps={len(missing)} out_of_order_gaps={len(out_of_order)} "
        f"frames={len(frames)} bad_crc={len(bad)} leftover={acc.hex(' ')}"
    )
    if gaps:
        print("missing_gaps:", [(round(t, 6), prev, cur, delta) for t, prev, cur, delta in missing[:20]])
        print(
            "out_of_order_gaps:",
            [(round(t, 6), prev, cur, delta, late) for t, prev, cur, delta, late in out_of_order[:20]],
        )
    print("ok_types:", dict(ok_counts.most_common()))
    for t, frame, payload, name, _, expected, got in bad[:50]:
        print(
            f"bad t={t:.6f} name={name} len={len(frame)} "
            f"got={got:02x} expect={expected:02x} raw={frame.hex(' ')} payload={payload.hex(' ')}"
        )


def main() -> None:
    if len(sys.argv) < 2:
        print("usage: analyze_prtp_pcap.py capture.pcap [matrix_host]", file=sys.stderr)
        raise SystemExit(2)
    matrix_host = sys.argv[2] if len(sys.argv) > 2 else "192.168.7.203"
    packets = read_prtp_packets(sys.argv[1], matrix_host)
    print(f"packets={len(packets)} matrix_host={matrix_host}")
    analyze_direction(packets, "in")
    analyze_direction(packets, "out")


if __name__ == "__main__":
    main()
