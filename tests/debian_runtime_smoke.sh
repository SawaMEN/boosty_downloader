#!/usr/bin/env bash
# Smoke-test release-shaped static binaries in fresh Debian containers.
# Does not test systemd or the installer; no real VPS or Boosty credentials.
set -euo pipefail

release="${1:-}"
server_binary="${2:?missing server binary}"
dbcheck_binary="${3:?missing dbcheck binary}"
case "$release" in bookworm|trixie) ;; *) echo "unsupported Debian release: $release" >&2; exit 2 ;; esac
test -f "$server_binary" && test -f "$dbcheck_binary"
command -v docker >/dev/null
command -v curl >/dev/null
command -v openssl >/dev/null

scratch="$(mktemp -d "${RUNNER_TEMP:-/tmp}/bdy-debian-${release}.XXXXXXXX")"
container="bdy-debian-${release}-$RANDOM-$$"
cleanup() {
  result=$?
  if [ "$result" -ne 0 ]; then
    echo "Debian smoke failed; container diagnostic log:" >&2
    docker logs "$container" >&2 || true
  fi
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf -- "$scratch"
  exit "$result"
}
trap cleanup EXIT

cp -- "$server_binary" "$scratch/boosty-downloader"
cp -- "$dbcheck_binary" "$scratch/boosty-dbcheck"
chmod 0700 "$scratch/boosty-downloader" "$scratch/boosty-dbcheck"
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$scratch/key.pem" -out "$scratch/cert.pem" -days 1 \
  -subj '/CN=localhost' >/dev/null 2>&1
chmod 0600 "$scratch/key.pem" "$scratch/cert.pem"
test_secret="$(openssl rand -base64 32)"
image="debian:${release}-slim"
docker pull "$image" >/dev/null

start_server() {
  docker run --detach --name "$container" \
    --user "$(id -u):$(id -g)" \
    --read-only --cap-drop ALL --security-opt no-new-privileges \
    --tmpfs /tmp:rw,noexec,nosuid,size=32m \
    --publish '127.0.0.1::18443' \
    --mount "type=bind,source=${scratch},target=/srv" \
    -e 'BD_LISTEN=:18443' \
    -e 'BD_DATA_DIR=/srv/state' \
    -e 'BD_ADMIN_USER=admin' \
    -e 'BD_ADMIN_PASSWORD=test-debian-password-123' \
    -e "BD_SECRET_KEY=$test_secret" \
    -e 'BD_TLS_CERT=/srv/cert.pem' \
    -e 'BD_TLS_KEY=/srv/key.pem' \
    "$image" /srv/boosty-downloader >/dev/null

  published="$(docker port "$container" 18443/tcp | head -n 1)"
  test -n "$published"
  port="${published##*:}"
  url="https://127.0.0.1:${port}"
  healthy=0
  for _ in $(seq 1 40); do
    if curl --silent --show-error --insecure --fail --max-time 2 \
      "$url/healthz" > /dev/null 2>&1; then
      healthy=1
      break
    fi
    if [ "$(docker inspect -f '{{.State.Running}}' "$container")" != "true" ]; then
      echo "Debian server exited before /healthz" >&2
      return 1
    fi
    sleep 0.25
  done
  if [ "$healthy" -ne 1 ]; then
    echo "Debian server never became healthy" >&2
    return 1
  fi
}

start_server
status="$(curl --silent --insecure -o /dev/null -w '%{http_code}' "$url/api/jobs")"
test "$status" = 401
curl --silent --show-error --insecure --fail \
  --cookie-jar "$scratch/cookies.txt" \
  -H 'Content-Type: application/json' -H 'X-BD-Request: 1' \
  --data '{"username":"admin","password":"test-debian-password-123"}' \
  "$url/api/account/login" >/dev/null
curl --silent --show-error --insecure --fail \
  --cookie "$scratch/cookies.txt" "$url/api/jobs" > /dev/null
test -s "$scratch/state/accounts.json"

# Account/session and archive metadata survive a real process restart.
docker rm -f "$container" >/dev/null
start_server
curl --silent --show-error --insecure --fail \
  --cookie "$scratch/cookies.txt" "$url/api/account" > /dev/null
curl --silent --show-error --insecure --fail \
  --cookie "$scratch/cookies.txt" "$url/api/jobs" > /dev/null

# The offline auxiliary executable also runs inside Debian.
docker rm -f "$container" >/dev/null
docker run --rm --user "$(id -u):$(id -g)" --read-only \
  --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=bind,source=${scratch},target=/srv" \
  "$image" /srv/boosty-dbcheck --help >/dev/null 2>&1

echo "Debian ${release}: HTTPS, access control, persistent login and dbcheck OK"
