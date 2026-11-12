#!/usr/bin/env bash
# Destructive failure-injection smoke for EPHEMERAL CI hosts ONLY.
set -Eeuo pipefail
umask 077

if [[ "${GITHUB_ACTIONS:-}" != true || "${BD_ROLLBACK_SMOKE:-}" != yes ]]; then
  echo "Refusing rollback failure injection outside explicit GitHub Actions CI." >&2
  exit 2
fi
[[ ${EUID:-$(id -u)} -eq 0 ]] || { echo "Requires root." >&2; exit 2; }

mode="${1:-}"
case "$mode" in native|docker) ;; *) echo "expected native or docker" >&2; exit 2 ;; esac

etc=/etc/boosty-downloader
data=/var/lib/boosty-downloader
project=/opt/boosty-downloader
bin=/usr/local/boosty-downloader/boosty-downloader
[[ "$(cat "$etc/mode")" == "$mode" ]] || { echo "Mode mismatch." >&2; exit 2; }
[[ "$(cat "$etc/public-host")" == 127.0.0.1 ]] || { echo "CI only: public host must be loopback." >&2; exit 2; }
port="$(sed -nE 's/^BD_PORT="?([0-9]+)"?$/\1/p' "$etc/environment" | head -1)"
[[ "$port" == 18443 ]] || { echo "CI only: expected port 18443." >&2; exit 2; }

health() { curl -kfSs --connect-timeout 2 --max-time 5 "https://127.0.0.1:$port/healthz" >/dev/null; }
health || { echo "Initial installed service is not healthy." >&2; exit 1; }

# A running installer holds this same lock. Updates and password rotations
# must fail without touching the live deployment.
exec 8>/run/lock/boosty-downloader-maintenance.lock
flock -n 8 || { echo "CI maintenance lock unexpectedly unavailable." >&2; exit 1; }
if /usr/local/bin/bdy update --force >/dev/null 2>&1; then
  echo "Updater ignored another process's maintenance lock." >&2
  exit 1
fi
if /usr/local/bin/bdy reset-password >/dev/null 2>&1; then
  echo "Password reset ignored another process's maintenance lock." >&2
  exit 1
fi
flock -u 8
exec 8>&-

# Run as service user; a durable user-owned file must survive broken releases.
sentinel="$data/.ci-rollback-sentinel"
printf 'CI archival bytes must survive a failed deployment\n' > "$sentinel"
chown boosty-downloader:boosty-downloader "$sentinel"
chmod 0600 "$sentinel"
before="$(sha256sum "$sentinel" | cut -d' ' -f1)"
config_before="$(sha256sum "$etc/environment" | cut -d' ' -f1)"
cert_before="$(sha256sum "$etc/tls/fullchain.pem" | cut -d' ' -f1)"
key_before="$(sha256sum "$etc/tls/privkey.pem" | cut -d' ' -f1)"

case "$mode" in
  native)
    [[ -s "$bin" ]] || { echo "Server binary missing." >&2; exit 1; }
    checker=/usr/local/bin/boosty-dbcheck
    [[ -s "$checker" ]] || { echo "Database verifier is missing." >&2; exit 1; }
    checksum_before="$(sha256sum "$checker" | cut -d' ' -f1)"
    cp -p "$checker" "$checker.prev"
    printf '#!/bin/sh\nexit 82\n' > "$checker.injected"
    chmod 0755 "$checker.injected"
    mv -f "$checker.injected" "$checker"
    cp -p "$bin" "$bin.prev"
    printf '#!/bin/sh\nexit 81\n' > "$bin.injected"
    chmod 0755 "$bin.injected"
    mv -f "$bin.injected" "$bin"
    systemctl restart boosty-downloader.service || true
    ;;
  docker)
    [[ -f "$project/compose.yaml" && -f "$project/.env" ]] || exit 1
    [[ ! -e "$project.prev" ]] || { echo "Prior rollback backup already exists." >&2; exit 1; }
    cp -a "$project" "$project.prev"
    docker tag boosty-downloader:local boosty-downloader:rollback
    # Inherit the available Alpine runtime, but use an entry point that exits.
    docker build -q -t boosty-downloader:local - <<'DOCKERFILE'
FROM boosty-downloader:rollback
ENTRYPOINT ["/bin/sh", "-c", "exit 81"]
DOCKERFILE
    docker compose --project-name boosty-downloader --project-directory "$project" \
      -f "$project/compose.yaml" up -d --no-build --force-recreate || true
    ;;
esac

# Broken runtime should NOT report health.
sleep 3
if health; then
  echo "Failed runtime unexpectedly passed health check; cannot validate rollback." >&2
  exit 1
fi

# Manager invokes the root-owned updater's rollback function and must
# restore the previous binary/image/project without touching user metadata.
timeout 130s /usr/local/bin/bdy rollback
health || { echo "Service is still unhealthy after rollback." >&2; exit 1; }
if [[ "$mode" == native ]]; then
  [[ "$(sha256sum "$checker" | cut -d' ' -f1)" == "$checksum_before" ]] ||
    { echo "Database verifier was not restored to the matching version." >&2; exit 1; }
fi

[[ "$(sha256sum "$sentinel" | cut -d' ' -f1)" == "$before" ]] ||
  { echo "Archived user bytes changed during rollback." >&2; exit 1; }
[[ "$(sha256sum "$etc/environment" | cut -d' ' -f1)" == "$config_before" ]] ||
  { echo "Secrets/configuration changed during rollback." >&2; exit 1; }
[[ "$(sha256sum "$etc/tls/fullchain.pem" | cut -d' ' -f1)" == "$cert_before" ]] ||
  { echo "TLS certificate changed during rollback." >&2; exit 1; }
[[ "$(sha256sum "$etc/tls/privkey.pem" | cut -d' ' -f1)" == "$key_before" ]] ||
  { echo "TLS private key changed during rollback." >&2; exit 1; }

python3 - "$data/update-status.json" <<'PY'
import json,sys
with open(sys.argv[1],encoding='utf-8') as f:
    result=json.load(f)
assert result.get('state')=='rolled_back', result
PY

rm -f -- "$sentinel"
echo "Rollback verified: $mode restored after an unhealthy release; archives, config and TLS unchanged."
