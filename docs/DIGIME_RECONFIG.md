# DigiME Reconfiguration Utility

`scripts/digime_reconfig.py` backs up, reconfigures, and restores DigiME / Digi Connect UART-IP modules through Digi RCI over HTTP.

The default deployment target is:

- devices: `192.168.7.200-203`
- VM/capture IP: `192.168.7.113`
- destination ports: `8087,8088,8089,8090`
- admin credentials: `root/dbps`

The deployment updates only the PRTP-relevant RCI groups:

- `profile.profile_type = udp_sockets`
- `udp_serial.count = 268`
- `udp_serial.dest[1] = <target-ip>:<port>`
- `udp_serial.dest[2..64] = off`
- `udp_server.port = 8087`

The original settings should be backed up before applying changes.

## Backup

Run from a host that can reach the DigiME management IPs:

```bash
scripts/digime_reconfig.py backup --devices 192.168.7.200-203
```

This creates `digime/backups/<timestamp>/` with one XML file per device plus `manifest.json`.

## Dry Run Deploy

```bash
scripts/digime_reconfig.py deploy \
  --devices 192.168.7.200-203 \
  --target-ip 192.168.7.113
```

Without `--yes`, deploy is a dry run. It queries and backs up live device settings, then writes the exact RCI update XML under the backup directory.

## Apply Deploy

```bash
scripts/digime_reconfig.py deploy \
  --devices 192.168.7.200-203 \
  --target-ip 192.168.7.113 \
  --yes
```

For a larger matrix, extend the range and either let ports auto-increment from `--base-port`, or provide explicit ports:

```bash
scripts/digime_reconfig.py deploy \
  --devices 192.168.7.200-207 \
  --target-ip 192.168.7.113 \
  --ports 8087,8088,8089,8090,8091,8092,8093,8094 \
  --yes
```

## Restore

First do a dry-run restore to emit the split RCI restore chunks:

```bash
scripts/digime_reconfig.py restore \
  --backup-dir digime/backups/<timestamp> \
  --devices 192.168.7.200-203
```

Then apply:

```bash
scripts/digime_reconfig.py restore \
  --backup-dir digime/backups/<timestamp> \
  --devices 192.168.7.200-203 \
  --yes
```

Restore splits the saved settings into multiple RCI requests so it stays below the Digi 32KB request limit.

## Offline Render

To render update XML from `digime/backup.cfg` without contacting hardware:

```bash
scripts/digime_reconfig.py render \
  --devices 192.168.7.200-203 \
  --target-ip 192.168.7.113 \
  --output-dir digime/rendered
```
