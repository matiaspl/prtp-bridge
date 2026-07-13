#!/usr/bin/env python3
"""
Reconfigure DigiME/Digi Connect UART-IP modules for PRTP capture fan-out.

The devices use Digi RCI XML over HTTP. This tool can:
  - back up current settings from a range of devices
  - deploy UDP serial destinations to one VM IP and per-device UDP ports
  - restore previously saved settings

Deploy and restore are dry-run by default; pass --yes to write devices.
"""

from __future__ import annotations

import argparse
import base64
import copy
import datetime as dt
import ipaddress
import json
import os
import sys
import textwrap
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET
from pathlib import Path


DEFAULT_DEVICES = "192.168.7.200-203"
DEFAULT_TARGET_IP = "192.168.7.113"
DEFAULT_USER = "root"
DEFAULT_PASSWORD = "dbps"
DEFAULT_RCI_PATH = "/UE/rci"
DEFAULT_BASE_PORT = 8087
DEFAULT_DEST_COUNT = 64
DEFAULT_MAX_REQUEST_BYTES = 30_000


class DigiError(RuntimeError):
    pass


def parse_host_range(spec: str) -> list[str]:
    hosts: list[str] = []
    for raw_part in spec.split(","):
        part = raw_part.strip()
        if not part:
            continue
        if "-" not in part:
            hosts.append(str(ipaddress.ip_address(part)))
            continue
        prefix, end_text = part.rsplit(".", 1)
        if "-" not in end_text:
            hosts.append(str(ipaddress.ip_address(part)))
            continue
        start_text, stop_text = end_text.split("-", 1)
        start = int(start_text)
        stop = int(stop_text)
        if start > stop:
            raise ValueError(f"invalid descending host range {part!r}")
        for last in range(start, stop + 1):
            hosts.append(str(ipaddress.ip_address(f"{prefix}.{last}")))
    if not hosts:
        raise ValueError("device list is empty")
    return hosts


def parse_ports(spec: str | None, base_port: int, count: int) -> list[int]:
    if spec:
        ports = [int(p.strip()) for p in spec.split(",") if p.strip()]
        if len(ports) != count:
            raise ValueError(f"--ports must contain exactly {count} ports")
    else:
        ports = [base_port + i for i in range(count)]
    for port in ports:
        if port < 1 or port > 65535:
            raise ValueError(f"invalid UDP port {port}")
    if len(set(ports)) != len(ports):
        raise ValueError("ports must be unique")
    return ports


def utc_stamp() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("%Y%m%d-%H%M%SZ")


def sanitize_host(host: str) -> str:
    return host.replace(":", "_").replace("/", "_")


def rci_envelope(kind: str, children: list[ET.Element]) -> bytes:
    root = ET.Element("rci_request", {"version": "1.1"})
    body = ET.SubElement(root, kind)
    for child in children:
        body.append(copy.deepcopy(child))
    return ET.tostring(root, encoding="utf-8", xml_declaration=False)


def rci_query_request() -> bytes:
    return b'<rci_request version="1.1"><query_setting/></rci_request>'


def rci_url(scheme: str, host: str, path: str) -> str:
    return f"{scheme}://{host}{path}"


def post_rci(
    host: str,
    payload: bytes,
    *,
    scheme: str,
    path: str,
    user: str,
    password: str,
    timeout: float,
) -> bytes:
    url = rci_url(scheme, host, path)
    req = urllib.request.Request(url, data=payload, method="POST")
    req.add_header("Content-Type", "text/xml")
    token = base64.b64encode(f"{user}:{password}".encode("utf-8")).decode("ascii")
    req.add_header("Authorization", f"Basic {token}")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.read()
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", "replace")
        raise DigiError(f"{host}: HTTP {exc.code} from {url}: {body[:500]}") from exc
    except urllib.error.URLError as exc:
        raise DigiError(f"{host}: {exc.reason}") from exc


def parse_xml(data: bytes | str, source: str) -> ET.Element:
    try:
        return ET.fromstring(data)
    except ET.ParseError as exc:
        raise DigiError(f"{source}: invalid XML: {exc}") from exc


def assert_no_rci_error(host: str, response: bytes) -> None:
    root = parse_xml(response, host)
    errors = list(root.iter("error"))
    if not errors:
        return
    details = []
    for err in errors:
        text = "".join(err.itertext()).strip()
        details.append(f"{err.attrib} {text}".strip())
    raise DigiError(f"{host}: RCI error: {'; '.join(details)}")


def find_settings_container(root: ET.Element) -> ET.Element:
    if root.tag in {"set_setting", "query_setting"}:
        return root
    for tag in ("set_setting", "query_setting"):
        found = root.find(tag)
        if found is not None:
            return found
    raise DigiError(f"could not find set_setting/query_setting in root <{root.tag}>")


def load_setting_groups(path: Path) -> list[ET.Element]:
    root = parse_xml(path.read_bytes(), str(path))
    container = find_settings_container(root)
    return [copy.deepcopy(child) for child in list(container)]


def find_first_group(groups: list[ET.Element], tag: str) -> ET.Element | None:
    for group in groups:
        if group.tag == tag:
            return group
    return None


def set_child_text(parent: ET.Element, tag: str, text: str) -> ET.Element:
    child = parent.find(tag)
    if child is None:
        child = ET.SubElement(parent, tag)
    child.text = text
    return child


def get_or_create_indexed(parent: ET.Element, tag: str, index: int) -> ET.Element:
    wanted = str(index)
    for child in parent.findall(tag):
        if child.attrib.get("index", "1") == wanted:
            return child
    return ET.SubElement(parent, tag, {"index": wanted})


def configure_udp_serial(
    groups: list[ET.Element],
    *,
    target_ip: str,
    target_port: int,
    dest_count: int,
    desc: str,
    listen_port: int,
) -> list[ET.Element]:
    template = {group.tag: copy.deepcopy(group) for group in groups}

    profile = copy.deepcopy(template.get("profile", ET.Element("profile")))
    set_child_text(profile, "profile_type", "udp_sockets")

    udp_serial = copy.deepcopy(template.get("udp_serial", ET.Element("udp_serial")))
    set_child_text(udp_serial, "state", "on")
    set_child_text(udp_serial, "trigger_on_pattern", "on")
    set_child_text(udp_serial, "pattern", "end")
    set_child_text(udp_serial, "strip_pattern", "on")
    set_child_text(udp_serial, "trigger_on_timeout", "off")
    set_child_text(udp_serial, "timeout", "1000")
    set_child_text(udp_serial, "count", "268")
    set_child_text(udp_serial, "socketid_state", "off")
    set_child_text(udp_serial, "closetime", "0")
    for idx in range(1, dest_count + 1):
        dest = get_or_create_indexed(udp_serial, "dest", idx)
        if idx == 1:
            set_child_text(dest, "state", "on")
            set_child_text(dest, "desc", desc)
            set_child_text(dest, "address", target_ip)
            set_child_text(dest, "port", str(target_port))
        else:
            set_child_text(dest, "state", "off")
            set_child_text(dest, "desc", "")
            set_child_text(dest, "address", "0.0.0.0")
            set_child_text(dest, "port", "0")

    udp_server = copy.deepcopy(template.get("udp_server", ET.Element("udp_server")))
    set_child_text(udp_server, "state", "on")
    set_child_text(udp_server, "port", str(listen_port))
    if udp_server.find("desc") is None:
        set_child_text(udp_server, "desc", "Serial/UDP Server (Port 1)")

    return [profile, udp_serial, udp_server]


def configure_udp_serial_framing(
    groups: list[ET.Element],
    *,
    mode: str,
    count: int,
    timeout_ms: int,
    pattern: str,
) -> ET.Element:
    udp_serial = copy.deepcopy(find_first_group(groups, "udp_serial") or ET.Element("udp_serial"))
    set_child_text(udp_serial, "state", "on")
    set_child_text(udp_serial, "count", str(count))
    set_child_text(udp_serial, "trigger_on_timeout", "off")
    set_child_text(udp_serial, "timeout", str(timeout_ms))
    if mode == "count":
        set_child_text(udp_serial, "trigger_on_pattern", "off")
        set_child_text(udp_serial, "pattern", "")
        set_child_text(udp_serial, "strip_pattern", "off")
    elif mode == "pattern":
        set_child_text(udp_serial, "trigger_on_pattern", "on")
        set_child_text(udp_serial, "pattern", pattern)
        set_child_text(udp_serial, "strip_pattern", "on")
    else:
        raise ValueError(f"unsupported framing mode {mode!r}")
    return udp_serial


def split_set_groups(groups: list[ET.Element], max_bytes: int) -> list[bytes]:
    chunks: list[bytes] = []
    current: list[ET.Element] = []
    for group in groups:
        candidate = current + [group]
        payload = rci_envelope("set_setting", candidate)
        if len(payload) <= max_bytes:
            current = candidate
            continue
        if current:
            chunks.append(rci_envelope("set_setting", current))
            current = [group]
            payload = rci_envelope("set_setting", current)
        if len(payload) > max_bytes:
            raise DigiError(
                f"single <{group.tag}> group is {len(payload)} bytes, larger than max request {max_bytes}"
            )
    if current:
        chunks.append(rci_envelope("set_setting", current))
    return chunks


def default_backup_dir(root: Path) -> Path:
    return root / "digime" / "backups" / utc_stamp()


def query_device_settings(host: str, args: argparse.Namespace) -> bytes:
    response = post_rci(
        host,
        rci_query_request(),
        scheme=args.scheme,
        path=args.rci_path,
        user=args.user,
        password=args.password,
        timeout=args.timeout,
    )
    assert_no_rci_error(host, response)
    return response


def save_backup(host: str, backup_dir: Path, raw: bytes) -> Path:
    backup_dir.mkdir(parents=True, exist_ok=True)
    path = backup_dir / f"{sanitize_host(host)}.xml"
    path.write_bytes(raw)
    return path


def write_manifest(path: Path, data: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def command_backup(args: argparse.Namespace) -> int:
    hosts = parse_host_range(args.devices)
    backup_dir = Path(args.backup_dir) if args.backup_dir else default_backup_dir(Path.cwd())
    manifest = {
        "created_utc": utc_stamp(),
        "action": "backup",
        "devices": [],
        "rci_path": args.rci_path,
    }
    for host in hosts:
        print(f"[backup] {host}")
        raw = query_device_settings(host, args)
        path = save_backup(host, backup_dir, raw)
        manifest["devices"].append({"host": host, "file": path.name})
        print(f"  saved {path}")
    write_manifest(backup_dir / "manifest.json", manifest)
    print(f"backup complete: {backup_dir}")
    return 0


def command_deploy(args: argparse.Namespace) -> int:
    hosts = parse_host_range(args.devices)
    ports = parse_ports(args.ports, args.base_port, len(hosts))
    target_ip = str(ipaddress.ip_address(args.target_ip))
    backup_dir = Path(args.backup_dir) if args.backup_dir else default_backup_dir(Path.cwd())
    plan_dir = Path(args.plan_dir) if args.plan_dir else backup_dir / "planned-updates"
    template_groups = load_setting_groups(Path(args.template))
    manifest = {
        "created_utc": utc_stamp(),
        "action": "deploy",
        "target_ip": target_ip,
        "devices": [],
        "dry_run": not args.yes,
    }

    for offset, (host, port) in enumerate(zip(hosts, ports)):
        print(f"[deploy] {host} -> {target_ip}:{port}")
        backup_path: Path | None = None
        try:
            raw = query_device_settings(host, args)
            groups = load_groups_from_bytes(raw, host)
            if not args.no_backup:
                backup_path = save_backup(host, backup_dir, raw)
                print(f"  backup saved {backup_path}")
        except Exception:
            if not args.use_template_on_query_failure:
                raise
            print("  query failed; using template", file=sys.stderr)
            groups = [copy.deepcopy(group) for group in template_groups]
            raw = b""
            backup_path = None

        if find_first_group(groups, "udp_serial") is None:
            groups = [copy.deepcopy(group) for group in template_groups]

        desc = args.desc_template.format(
            host=host,
            target_ip=target_ip,
            port=port,
            index=offset,
            last_octet=host.split(".")[-1],
        )
        update_groups = configure_udp_serial(
            groups,
            target_ip=target_ip,
            target_port=port,
            dest_count=args.dest_count,
            desc=desc,
            listen_port=args.listen_port,
        )
        payload = rci_envelope("set_setting", update_groups)
        plan_dir.mkdir(parents=True, exist_ok=True)
        plan_path = plan_dir / f"{sanitize_host(host)}-to-{port}.xml"
        plan_path.write_bytes(payload)
        manifest["devices"].append(
            {
                "host": host,
                "target_ip": target_ip,
                "target_port": port,
                "plan": str(plan_path.relative_to(backup_dir) if plan_path.is_relative_to(backup_dir) else plan_path),
                "backup": backup_path.name if backup_path is not None else None,
            }
        )
        if not args.yes:
            print(f"  dry-run wrote {plan_path}")
            continue
        response = post_rci(
            host,
            payload,
            scheme=args.scheme,
            path=args.rci_path,
            user=args.user,
            password=args.password,
            timeout=args.timeout,
        )
        assert_no_rci_error(host, response)
        print("  applied")

    write_manifest(backup_dir / "manifest.json", manifest)
    if not args.yes:
        print("dry-run complete; pass --yes to apply these updates")
    else:
        print("deploy complete")
    return 0


def load_groups_from_bytes(raw: bytes, source: str) -> list[ET.Element]:
    root = parse_xml(raw, source)
    container = find_settings_container(root)
    return [copy.deepcopy(child) for child in list(container)]


def backup_file_for_host(backup_dir: Path, host: str) -> Path:
    direct = backup_dir / f"{sanitize_host(host)}.xml"
    if direct.exists():
        return direct
    matches = sorted(backup_dir.glob(f"{sanitize_host(host)}*.xml"))
    if matches:
        return matches[0]
    raise DigiError(f"no backup XML found for {host} in {backup_dir}")


def command_restore(args: argparse.Namespace) -> int:
    hosts = parse_host_range(args.devices)
    backup_dir = Path(args.backup_dir)
    if not backup_dir.exists():
        raise DigiError(f"backup directory does not exist: {backup_dir}")
    for host in hosts:
        path = backup_file_for_host(backup_dir, host)
        groups = load_setting_groups(path)
        chunks = split_set_groups(groups, args.max_request_bytes)
        print(f"[restore] {host} from {path} in {len(chunks)} chunk(s)")
        if not args.yes:
            for i, payload in enumerate(chunks, 1):
                out = backup_dir / f"restore-{sanitize_host(host)}-{i:02d}.xml"
                out.write_bytes(payload)
            print("  dry-run wrote restore chunks; pass --yes to apply")
            continue
        for i, payload in enumerate(chunks, 1):
            print(f"  applying chunk {i}/{len(chunks)} ({len(payload)} bytes)")
            response = post_rci(
                host,
                payload,
                scheme=args.scheme,
                path=args.rci_path,
                user=args.user,
                password=args.password,
                timeout=args.timeout,
            )
            assert_no_rci_error(host, response)
        print("  restored")
    return 0


def command_framing(args: argparse.Namespace) -> int:
    hosts = parse_host_range(args.devices)
    backup_dir = Path(args.backup_dir) if args.backup_dir else default_backup_dir(Path.cwd())
    plan_dir = Path(args.plan_dir) if args.plan_dir else backup_dir / "planned-updates"
    manifest = {
        "created_utc": utc_stamp(),
        "action": "framing",
        "mode": args.mode,
        "devices": [],
        "dry_run": not args.yes,
    }

    for host in hosts:
        print(f"[framing] {host} mode={args.mode}")
        raw = query_device_settings(host, args)
        groups = load_groups_from_bytes(raw, host)
        backup_path: Path | None = None
        if not args.no_backup:
            backup_path = save_backup(host, backup_dir, raw)
            print(f"  backup saved {backup_path}")

        udp_serial = configure_udp_serial_framing(
            groups,
            mode=args.mode,
            count=args.count,
            timeout_ms=args.timeout_ms,
            pattern=args.pattern,
        )
        payload = rci_envelope("set_setting", [udp_serial])
        plan_dir.mkdir(parents=True, exist_ok=True)
        plan_path = plan_dir / f"{sanitize_host(host)}-framing-{args.mode}.xml"
        plan_path.write_bytes(payload)
        manifest["devices"].append(
            {
                "host": host,
                "plan": str(plan_path.relative_to(backup_dir) if plan_path.is_relative_to(backup_dir) else plan_path),
                "backup": backup_path.name if backup_path is not None else None,
            }
        )
        if not args.yes:
            print(f"  dry-run wrote {plan_path}")
            continue
        response = post_rci(
            host,
            payload,
            scheme=args.scheme,
            path=args.rci_path,
            user=args.user,
            password=args.password,
            timeout=args.timeout,
        )
        assert_no_rci_error(host, response)
        print("  applied")

    write_manifest(backup_dir / "manifest.json", manifest)
    if not args.yes:
        print("dry-run complete; pass --yes to apply these updates")
    else:
        print("framing update complete")
    return 0


def command_render(args: argparse.Namespace) -> int:
    hosts = parse_host_range(args.devices)
    ports = parse_ports(args.ports, args.base_port, len(hosts))
    target_ip = str(ipaddress.ip_address(args.target_ip))
    groups = load_setting_groups(Path(args.template))
    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    for offset, (host, port) in enumerate(zip(hosts, ports)):
        desc = args.desc_template.format(
            host=host,
            target_ip=target_ip,
            port=port,
            index=offset,
            last_octet=host.split(".")[-1],
        )
        payload = rci_envelope(
            "set_setting",
            configure_udp_serial(
                groups,
                target_ip=target_ip,
                target_port=port,
                dest_count=args.dest_count,
                desc=desc,
                listen_port=args.listen_port,
            ),
        )
        path = output_dir / f"{sanitize_host(host)}-to-{port}.xml"
        path.write_bytes(payload)
        print(path)
    return 0


def add_common_network_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--devices", default=DEFAULT_DEVICES, help=f"device IPs/ranges, default {DEFAULT_DEVICES}")
    parser.add_argument("--scheme", default="http", choices=["http", "https"], help="RCI URL scheme")
    parser.add_argument("--rci-path", default=DEFAULT_RCI_PATH, help=f"RCI HTTP path, default {DEFAULT_RCI_PATH}")
    parser.add_argument("--user", default=os.environ.get("DIGIME_USER", DEFAULT_USER), help="admin user")
    parser.add_argument("--password", default=os.environ.get("DIGIME_PASSWORD", DEFAULT_PASSWORD), help="admin password")
    parser.add_argument("--timeout", type=float, default=5.0, help="HTTP timeout in seconds")


def add_mapping_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--target-ip", default=DEFAULT_TARGET_IP, help=f"VM/capture IP, default {DEFAULT_TARGET_IP}")
    parser.add_argument("--base-port", type=int, default=DEFAULT_BASE_PORT, help=f"first target UDP port, default {DEFAULT_BASE_PORT}")
    parser.add_argument("--ports", help="comma-separated explicit target UDP ports; count must match devices")
    parser.add_argument("--listen-port", type=int, default=8087, help="DigiME UDP server listen port to preserve/set")
    parser.add_argument("--dest-count", type=int, default=DEFAULT_DEST_COUNT, help="number of udp_serial destination slots to manage")
    parser.add_argument(
        "--desc-template",
        default="PRTP-VM-{last_octet}",
        help="description for enabled dest; fields: host,target_ip,port,index,last_octet",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Back up, reconfigure, and restore DigiME UART-IP RCI settings.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=textwrap.dedent(
            """
            Examples:
              scripts/digime_reconfig.py backup --devices 192.168.7.200-203
              scripts/digime_reconfig.py deploy --target-ip 192.168.7.113 --devices 192.168.7.200-203
              scripts/digime_reconfig.py deploy --target-ip 192.168.7.113 --ports 8087,8088,8089,8090 --yes
              scripts/digime_reconfig.py restore --backup-dir digime/backups/20260514-120000Z --yes
            """
        ),
    )
    sub = parser.add_subparsers(dest="command", required=True)

    backup = sub.add_parser("backup", help="query and save current device settings")
    add_common_network_args(backup)
    backup.add_argument("--backup-dir", help="output backup directory")
    backup.set_defaults(func=command_backup)

    deploy = sub.add_parser("deploy", help="set per-device UDP serial destinations to one target IP")
    add_common_network_args(deploy)
    add_mapping_args(deploy)
    deploy.add_argument("--template", default="digime/backup.cfg", help="fallback/template config XML")
    deploy.add_argument("--backup-dir", help="backup and manifest directory")
    deploy.add_argument("--plan-dir", help="directory for planned update XML files")
    deploy.add_argument("--no-backup", action="store_true", help="do not back up before applying")
    deploy.add_argument("--use-template-on-query-failure", action="store_true", help="render/apply from template if live query fails")
    deploy.add_argument("--yes", action="store_true", help="actually write device settings")
    deploy.set_defaults(func=command_deploy)

    restore = sub.add_parser("restore", help="restore settings from a backup directory")
    add_common_network_args(restore)
    restore.add_argument("--backup-dir", required=True, help="backup directory containing per-device XML files")
    restore.add_argument("--max-request-bytes", type=int, default=DEFAULT_MAX_REQUEST_BYTES, help="max RCI set request size")
    restore.add_argument("--yes", action="store_true", help="actually write device settings")
    restore.set_defaults(func=command_restore)

    framing = sub.add_parser("framing", help="switch UDP serial packet framing without changing destinations")
    add_common_network_args(framing)
    framing.add_argument("--backup-dir", help="backup and manifest directory")
    framing.add_argument("--plan-dir", help="directory for planned update XML files")
    framing.add_argument("--mode", choices=["count", "pattern"], required=True, help="count disables end-pattern framing; pattern restores pattern=end")
    framing.add_argument("--count", type=int, default=268, help="UDP serial byte-count trigger, default 268")
    framing.add_argument("--pattern", default="end", help="end pattern for pattern mode, default 'end'")
    framing.add_argument("--timeout-ms", type=int, default=1000, help="preserved timeout value while timeout trigger is off")
    framing.add_argument("--no-backup", action="store_true", help="do not back up before applying")
    framing.add_argument("--yes", action="store_true", help="actually write device settings")
    framing.set_defaults(func=command_framing)

    render = sub.add_parser("render", help="render deploy XML from template without contacting devices")
    add_mapping_args(render)
    render.add_argument("--devices", default=DEFAULT_DEVICES, help=f"device IPs/ranges, default {DEFAULT_DEVICES}")
    render.add_argument("--template", default="digime/backup.cfg", help="template config XML")
    render.add_argument("--output-dir", default="digime/rendered", help="output directory")
    render.set_defaults(func=command_render)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except (DigiError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
