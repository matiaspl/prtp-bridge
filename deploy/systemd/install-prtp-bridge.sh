#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat >&2 <<'EOF'
Usage:
  install-prtp-bridge.sh [options]

Installs the config-driven prtp-bridge deployment:
  - creates the service user (default: prtp) as a system account in the audio group
  - backs up and removes the old kroma-prtp-net startup units/env/script
  - installs prtp-bridge and prtp-matrix-helper binaries
  - installs /etc/kroma/prtp-bridge.json
  - installs and enables the new matrix-helper service and NET0-NET3 bridge target

Options:
  --bridge-bin PATH      prebuilt prtp-bridge binary
  --helper-bin PATH      prebuilt prtp-matrix-helper binary
  --config PATH          config JSON to install (default: deploy/systemd/prtp-bridge.json)
  --install-dir PATH     binary install directory (default: /opt/kroma)
  --etc-dir PATH         config install directory (default: /etc/kroma)
  --unit-dir PATH        systemd unit directory (default: /etc/systemd/system)
  --backup-dir PATH      backup root (default: /var/backups/prtp-bridge/<timestamp>)
  --user NAME            service account to run as (default: prtp, created if absent)
  --no-start             install and enable units but do not start them
  --no-build             require --bridge-bin and --helper-bin instead of building from source
  -h, --help             show this help
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

info() {
	echo "$*" >&2
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

BRIDGE_BIN=""
HELPER_BIN=""
CONFIG_SRC="$SCRIPT_DIR/prtp-bridge.json"
INSTALL_DIR="/opt/kroma"
ETC_DIR="/etc/kroma"
UNIT_DIR="/etc/systemd/system"
BACKUP_DIR=""
SERVICE_USER="prtp"
START_UNITS=1
ALLOW_BUILD=1

while [[ $# -gt 0 ]]; do
	case "$1" in
		--bridge-bin)
			BRIDGE_BIN="${2:-}"
			shift 2
			;;
		--helper-bin)
			HELPER_BIN="${2:-}"
			shift 2
			;;
		--config)
			CONFIG_SRC="${2:-}"
			shift 2
			;;
		--install-dir)
			INSTALL_DIR="${2:-}"
			shift 2
			;;
		--etc-dir)
			ETC_DIR="${2:-}"
			shift 2
			;;
		--unit-dir)
			UNIT_DIR="${2:-}"
			shift 2
			;;
		--backup-dir)
			BACKUP_DIR="${2:-}"
			shift 2
			;;
		--user)
			SERVICE_USER="${2:-}"
			[[ -n "$SERVICE_USER" ]] || die "--user requires a non-empty name"
			shift 2
			;;
		--no-start)
			START_UNITS=0
			shift
			;;
		--no-build)
			ALLOW_BUILD=0
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			usage
			die "unknown option: $1"
			;;
	esac
done

[[ $EUID -eq 0 ]] || die "run as root, for example: sudo $0"
[[ -f "$CONFIG_SRC" ]] || die "config file not found: $CONFIG_SRC"
[[ -f "$SCRIPT_DIR/prtp-matrix-helper.service" ]] || die "missing helper unit next to installer"
[[ -f "$SCRIPT_DIR/prtp-bridge@.service" ]] || die "missing bridge template unit next to installer"
[[ -f "$SCRIPT_DIR/prtp-bridge.target" ]] || die "missing bridge target unit next to installer"

# Ensure the service user exists as a system account in the audio group.
if ! id "$SERVICE_USER" &>/dev/null; then
	info "creating system user '$SERVICE_USER' (audio group)"
	useradd \
		--system \
		--no-create-home \
		--shell /usr/sbin/nologin \
		--comment "prtp-bridge service account" \
		"$SERVICE_USER"
else
	info "user '$SERVICE_USER' already exists"
fi
# Ensure the user is in the audio group (idempotent).
if ! id -nG "$SERVICE_USER" | grep -qw audio; then
	info "adding '$SERVICE_USER' to audio group"
	usermod --append --groups audio "$SERVICE_USER"
fi

BUILD_DIR=""
UNIT_TMP=""
cleanup() {
	[[ -n "$BUILD_DIR" ]] && rm -rf "$BUILD_DIR"
	[[ -n "$UNIT_TMP" ]] && rm -rf "$UNIT_TMP"
}
trap cleanup EXIT

if [[ -z "$BRIDGE_BIN" || -z "$HELPER_BIN" ]]; then
	if [[ "$ALLOW_BUILD" -eq 0 ]]; then
		die "--bridge-bin and --helper-bin are required with --no-build"
	fi
	[[ -f "$REPO_ROOT/go.mod" ]] || die "cannot build: go.mod not found at $REPO_ROOT"
	command -v go >/dev/null 2>&1 || die "cannot build: go is not in PATH"
	BUILD_DIR="$(mktemp -d)"
	info "building prtp-bridge and prtp-matrix-helper with $(go version)"
	(
		cd "$REPO_ROOT"
		go build -o "$BUILD_DIR/prtp-bridge" ./cmd/prtp-bridge
		go build -o "$BUILD_DIR/prtp-matrix-helper" ./cmd/prtp-matrix-helper
	)
	BRIDGE_BIN="$BUILD_DIR/prtp-bridge"
	HELPER_BIN="$BUILD_DIR/prtp-matrix-helper"
fi

[[ -x "$BRIDGE_BIN" ]] || die "bridge binary is missing or not executable: $BRIDGE_BIN"
[[ -x "$HELPER_BIN" ]] || die "matrix helper binary is missing or not executable: $HELPER_BIN"

if [[ -z "$BACKUP_DIR" ]]; then
	BACKUP_DIR="/var/backups/prtp-bridge/$(date -u +%Y%m%dT%H%M%SZ)"
fi
mkdir -p "$BACKUP_DIR"

backup_path() {
	local path="$1"
	if [[ -e "$path" || -L "$path" ]]; then
		mkdir -p "$(dirname "$BACKUP_DIR$path")"
		cp -a "$path" "$BACKUP_DIR$path"
		info "backed up $path"
	fi
}

remove_path() {
	local path="$1"
	if [[ -e "$path" || -L "$path" ]]; then
		rm -f "$path"
		info "removed $path"
	fi
}

old_instances=(0 1 2 3 NET0 NET1 NET2 NET3)
new_instances=(NET0 NET1 NET2 NET3)

info "stopping old kroma-prtp-net services"
systemctl stop kroma-prtp-net.target >/dev/null 2>&1 || true
for inst in "${old_instances[@]}"; do
	systemctl stop "kroma-prtp-net@${inst}.service" >/dev/null 2>&1 || true
done
systemctl disable kroma-prtp-net.target >/dev/null 2>&1 || true
for inst in "${old_instances[@]}"; do
	systemctl disable "kroma-prtp-net@${inst}.service" >/dev/null 2>&1 || true
done

info "stopping new services before replacement"
systemctl stop kroma-prtp-bridge.target >/dev/null 2>&1 || true
for inst in "${new_instances[@]}"; do
	systemctl stop "kroma-prtp-bridge@${inst}.service" >/dev/null 2>&1 || true
done
systemctl stop prtp-bridge.target >/dev/null 2>&1 || true
for inst in "${new_instances[@]}"; do
	systemctl stop "prtp-bridge@${inst}.service" >/dev/null 2>&1 || true
done
systemctl stop kroma-prtp-matrix-helper.service >/dev/null 2>&1 || true
systemctl stop prtp-matrix-helper.service >/dev/null 2>&1 || true

backup_path "$UNIT_DIR/kroma-prtp-net.target"
backup_path "$UNIT_DIR/kroma-prtp-net@.service"
backup_path "$ETC_DIR/prtp-net.env"
backup_path "$INSTALL_DIR/prtp-net-instance.sh"

backup_path "$UNIT_DIR/kroma-prtp-matrix-helper.service"
backup_path "$UNIT_DIR/prtp-matrix-helper.service"
backup_path "$UNIT_DIR/kroma-prtp-bridge@.service"
backup_path "$UNIT_DIR/kroma-prtp-bridge.target"
backup_path "$UNIT_DIR/prtp-bridge@.service"
backup_path "$UNIT_DIR/prtp-bridge.target"
backup_path "$ETC_DIR/prtp-bridge.json"
backup_path "$INSTALL_DIR/prtp-bridge"
backup_path "$INSTALL_DIR/prtp-matrix-helper"

remove_path "$UNIT_DIR/kroma-prtp-net.target"
remove_path "$UNIT_DIR/kroma-prtp-net@.service"
remove_path "$ETC_DIR/prtp-net.env"
remove_path "$INSTALL_DIR/prtp-net-instance.sh"
remove_path "$UNIT_DIR/kroma-prtp-bridge@.service"
remove_path "$UNIT_DIR/kroma-prtp-bridge.target"
remove_path "$UNIT_DIR/kroma-prtp-matrix-helper.service"

install -d -m 0755 "$INSTALL_DIR" "$ETC_DIR" "$UNIT_DIR"
install -m 0755 "$BRIDGE_BIN" "$INSTALL_DIR/prtp-bridge"
install -m 0755 "$HELPER_BIN" "$INSTALL_DIR/prtp-matrix-helper"
install -m 0644 "$CONFIG_SRC" "$ETC_DIR/prtp-bridge.json"

# Substitute @@SERVICE_USER@@ placeholder in unit files before installing.
UNIT_TMP="$(mktemp -d)"
sed "s/@@SERVICE_USER@@/${SERVICE_USER}/g" \
	"$SCRIPT_DIR/prtp-matrix-helper.service" > "$UNIT_TMP/prtp-matrix-helper.service"
sed "s/@@SERVICE_USER@@/${SERVICE_USER}/g" \
	"$SCRIPT_DIR/prtp-bridge@.service" > "$UNIT_TMP/prtp-bridge@.service"
install -m 0644 "$UNIT_TMP/prtp-matrix-helper.service" "$UNIT_DIR/prtp-matrix-helper.service"
install -m 0644 "$UNIT_TMP/prtp-bridge@.service" "$UNIT_DIR/prtp-bridge@.service"
install -m 0644 "$SCRIPT_DIR/prtp-bridge.target" "$UNIT_DIR/prtp-bridge.target"
info "service user: $SERVICE_USER"

systemctl daemon-reload
systemctl enable prtp-matrix-helper.service prtp-bridge.target >/dev/null

if [[ "$START_UNITS" -eq 1 ]]; then
	info "starting prtp-matrix-helper.service and prtp-bridge.target"
	systemctl start prtp-matrix-helper.service
	systemctl start prtp-bridge.target
else
	info "installed and enabled units; startup skipped because --no-start was set"
fi

info "installed prtp-bridge deployment"
info "backup root: $BACKUP_DIR"
