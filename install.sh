#!/usr/bin/env bash
# Public, idempotent host-side installer. Do not modify archived user data.
set -Eeuo pipefail
umask 077

REPO='https://github.com/SawaMEN/boosty_downloader.git'
ETC=/etc/boosty-downloader
DATA=/var/lib/boosty-downloader
PROJECT=/opt/boosty-downloader
BIN=/usr/local/boosty-downloader/boosty-downloader
SERVICE=/etc/systemd/system/boosty-downloader.service
MODE=''
ROOT=''
WORK=''
STAGE=''
BACKUP_PROJECT=''
RESTORE_IMAGE=0
RELEASE_ID=''

say() { printf '%s\n' "$*"; }
die() { printf 'Ошибка: %s\n' "$*" >&2; exit 1; }
cleanup() {
  if [[ -n "$WORK" && -d "$WORK" ]]; then rm -rf -- "$WORK"; fi
  if [[ -n "$STAGE" && -d "$STAGE" ]]; then rm -rf -- "$STAGE"; fi
}
trap cleanup EXIT
usage() {
  cat <<'HELP'
Boosty Downloader — установка из публичного GitHub.
  sudo bash install.sh [--mode native|docker]
  sudo bash install.sh --help

Если install.sh скачан отдельно, исходники будут загружены автоматически.
Для установки без вопросов задайте BD_PUBLIC_HOST, BD_PORT, BD_TLS_MODE
и при необходимости BD_ADMIN_USER, BD_ADMIN_PASSWORD.
BD_BUILD_FROM_SOURCE=1  — собрать native локально (без скачивания релиза)
BD_NATIVE_BINARY + BD_NATIVE_SHA256 — собственный проверяемый бинарник
BD_SOURCE_REF=main       — ветка или тег для скачивания исходников
BD_VIDEO_ACCEL=none|amd|intel|nvidia — аппаратное ускорение (опционально)
Режим установки сохраняет прежние архивы, ключи и настройки.
HELP
}
while (($#)); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --mode)
      (($# >= 2)) || die 'Укажите native или docker после --mode'
      MODE="$2"; shift 2 ;;
    --mode=*) MODE="${1#--mode=}"; shift ;;
    native|docker)
      [[ -z "$MODE" ]] || die 'Режим указан дважды'
      MODE="$1"; shift ;;
    *) die "Неизвестный аргумент: $1 (используйте --help)" ;;
  esac
done
[[ ${EUID:-$(id -u)} -eq 0 ]] || die 'Запустите: sudo bash install.sh'
[[ "$(uname -s)" == Linux ]] || die 'Поддерживается только Linux'
command -v systemctl >/dev/null || die 'Требуется Linux с systemd'
[[ -d /run/systemd/system ]] || die 'Система не запущена с systemd'
command -v flock >/dev/null || die 'Требуется flock (util-linux)'
mkdir -p /run/lock
exec 9>/run/lock/boosty-downloader-maintenance.lock
flock -n 9 || die 'Другой процесс установки или обновления уже выполняется'
WORK="$(mktemp -d)"
# When invoked via curl | sudo bash, BASH_SOURCE[0] is unset under set -u.
# In that case there is no adjacent checkout; clone public sources below.
SCRIPT_PATH="${BASH_SOURCE[0]:-}"
STREAMED=0
if [[ -n "$SCRIPT_PATH" ]]; then
  ORIGIN="$(cd -- "$(dirname -- "$SCRIPT_PATH")" && pwd)"
else
  STREAMED=1
  ORIGIN="$(pwd -P)"
fi

install_packages() {
  (($#)) || return 0
  if command -v apt-get >/dev/null; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y "$@"
  elif command -v dnf >/dev/null; then dnf install -y "$@"
  elif command -v zypper >/dev/null; then zypper --non-interactive install "$@"
  elif command -v pacman >/dev/null; then pacman -Sy --noconfirm --needed "$@"
  else die "Установите системные пакеты вручную: $*"; fi
}
require_command() {
  local name="$1" package="$2"
  if ! command -v "$name" >/dev/null; then
    say "Установка зависимости: $package"
    install_packages "$package"
  fi
  command -v "$name" >/dev/null || die "Не найдена команда $name"
}
require_command curl curl
require_command openssl openssl
require_command python3 python3
require_command sha256sum coreutils

# Never accidentally install a stale checkout when the installer came via stdin.
if ((STREAMED == 0)) && [[ -f "$ORIGIN/go.mod" && -f "$ORIGIN/deploy/docker/compose.yaml" ]]; then
  ROOT="$ORIGIN"
else
  require_command git git
  ref="${BD_SOURCE_REF:-main}"
  [[ "$ref" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ && "$ref" != *..* ]] ||
    die 'Недопустимый BD_SOURCE_REF'
  say "Загрузка исходников публичного репозитория ($ref)..."
  git clone --depth 1 --single-branch --branch "$ref" "$REPO" "$WORK/source" ||
    die 'Не удалось скачать исходники из GitHub'
  ROOT="$WORK/source"
fi
[[ -f "$ROOT/go.mod" && -f "$ROOT/deploy/systemd/boosty-downloader.service" ]] ||
  die 'Отсутствуют файлы проекта'

oldmode="$(cat "$ETC/mode" 2>/dev/null || echo none)"
if [[ -z "$MODE" ]]; then
  if [[ "$oldmode" == native || "$oldmode" == docker ]]; then
    MODE="$oldmode"
    say "Обновление существующего режима: $MODE"
  else
    say 'Boosty Downloader — выбор установки'
    say '1) Native (systemd, без Docker)'
    say '2) Docker Compose'
    say '0) Выход'
    [[ -r /dev/tty ]] || die 'Без терминала укажите --mode native или --mode docker'
    read -r -p 'Выбор: ' choice </dev/tty
    case "$choice" in 1) MODE=native ;; 2) MODE=docker ;; 0) exit 0 ;; *) die 'Неверный выбор' ;; esac
  fi
fi
[[ "$MODE" == native || "$MODE" == docker ]] || die 'Режим: native или docker'

if [[ "$MODE" == docker ]]; then
  if ! command -v docker >/dev/null || ! docker compose version >/dev/null 2>&1; then
    say 'Требуется Docker Engine с Compose v2; проверка системных пакетов...'
    if command -v apt-get >/dev/null; then
      install_packages docker.io docker-compose-v2 ||
        install_packages docker.io docker-compose-plugin ||
        die 'Не удалось установить Docker Engine + Compose v2. Используйте официальный репозиторий Docker.'
    else
      die 'Установите Docker Engine и Docker Compose v2'
    fi
  fi
  require_command git git
  systemctl enable --now docker.service >/dev/null 2>&1 || true
  docker info >/dev/null 2>&1 || die 'Docker daemon недоступен'
  docker compose version >/dev/null || die 'Отсутствует Docker Compose v2'
fi

if [[ "$MODE" == native ]]; then
  require_command zstd zstd
fi
if ! id boosty-downloader >/dev/null 2>&1; then
  require_command useradd passwd
  useradd --system --home-dir "$DATA" --shell "$(command -v nologin || echo /usr/sbin/nologin)" boosty-downloader
fi
install -d -m 0750 -o root -g boosty-downloader "$ETC"
install -d -m 0700 -o boosty-downloader -g boosty-downloader "$DATA"
install -d -m 0750 -o root -g boosty-downloader "$ETC/tls"
[[ ! -L "$ETC/environment" && ! -L "$ETC/public-host" ]] ||
  die 'Симлинк в конфигурации /etc/boosty-downloader запрещён'

ask() {
  local label="$1" default="$2" answer=''
  if [[ -r /dev/tty ]]; then
    read -r -p "$label [$default]: " answer </dev/tty
  fi
  printf '%s' "${answer:-$default}"
}
valid_host() {
  python3 - "$1" <<'PY'
import ipaddress,re,sys
s=sys.argv[1]
try:
    ipaddress.ip_address(s)
    sys.exit(0)
except ValueError: pass
if not s or len(s)>253 or len(s)<3:
    sys.exit(1)
labels=s.split('.')
if len(labels)<2 or any(not re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?', p) for p in labels):
    sys.exit(1)
PY
}
valid_port() {
  [[ "$1" =~ ^[0-9]{1,5}$ ]] && ((10#$1 >= 1024 && 10#$1 <= 65535))
}
if [[ ! -f "$ETC/environment" ]]; then
  if [[ -n "${BD_PUBLIC_HOST:-}" ]]; then
    PUBLIC_HOST="$BD_PUBLIC_HOST"
  else
    say 'Адрес панели: 1) IP; 2) домен; 3) localhost/SSH-туннель'
    [[ -r /dev/tty ]] || die 'Для установки без терминала укажите BD_PUBLIC_HOST'
    read -r -p 'Выбор [3]: ' choice </dev/tty
    case "${choice:-3}" in
      1) read -r -p 'IP: ' PUBLIC_HOST </dev/tty ;;
      2) read -r -p 'Домен: ' PUBLIC_HOST </dev/tty ;;
      3) PUBLIC_HOST=127.0.0.1 ;;
      *) die 'Неверный выбор адреса' ;;
    esac
  fi
  valid_host "$PUBLIC_HOST" || die 'Недопустимый IP-адрес или домен'
  PORT="${BD_PORT:-}"
  if [[ -z "$PORT" ]]; then PORT="$(ask 'Порт HTTPS' 8443)"; fi
  valid_port "$PORT" || die 'Порт должен быть от 1024 до 65535'
  USERNAME="${BD_ADMIN_USER:-admin}"
  [[ "$USERNAME" =~ ^[A-Za-z0-9_.-]{1,32}$ ]] || die 'Недопустимый логин администратора'
  PASSWORD="${BD_ADMIN_PASSWORD:-$(openssl rand -hex 18)}"
  [[ ${#PASSWORD} -ge 12 && "$PASSWORD" =~ ^[A-Za-z0-9_.-]+$ ]] ||
    die 'Пароль: не менее 12 латинских букв/цифр'
  SECRET="$(openssl rand -base64 32 | tr -d '\n')"
  if [[ "$PUBLIC_HOST" == 127.0.0.1 || "$PUBLIC_HOST" == ::1 ]]; then LISTEN="127.0.0.1:$PORT"
  else LISTEN=":$PORT"; fi
  tmp_env="$WORK/environment"
  cat >"$tmp_env" <<ENV
BD_LISTEN="$LISTEN"
BD_PORT="$PORT"
BD_DATA_DIR="$DATA"
BD_ADMIN_USER="$USERNAME"
BD_ADMIN_PASSWORD="$PASSWORD"
BD_SECRET_KEY="$SECRET"
BD_TLS_CERT="$ETC/tls/fullchain.pem"
BD_TLS_KEY="$ETC/tls/privkey.pem"
ENV
  install -m 0640 -o root -g boosty-downloader "$tmp_env" "$ETC/environment"
  printf '%s\n' "$PUBLIC_HOST" > "$ETC/public-host"
  chown root:boosty-downloader "$ETC/public-host"
  chmod 0640 "$ETC/public-host"
  say "Администратор: $USERNAME"
  say "Первичный пароль: $PASSWORD"
  say 'Сохраните пароль. В дальнейшем он не будет выведен.'
else
  # Only root can modify this file (0640); preserve existing secrets.
  [[ -f "$ETC/environment" ]] || die 'Конфигурация должна быть обычным файлом'
  # shellcheck disable=SC1091
  set -a; source "$ETC/environment"; set +a
  PORT="${BD_PORT:-}"
  PUBLIC_HOST="$(cat "$ETC/public-host" 2>/dev/null || echo 127.0.0.1)"
  valid_port "$PORT" && valid_host "$PUBLIC_HOST" ||
    die 'Существующая конфигурация содержит неправильный адрес или порт'
  say 'Существующая конфигурация и учётные данные сохранены.'
fi
VIDEO_ACCEL="${BD_VIDEO_ACCEL:-$(cat "$ETC/video-mode" 2>/dev/null || echo none)}"
case "$VIDEO_ACCEL" in none|amd|intel|nvidia) ;; *) die 'BD_VIDEO_ACCEL: none, amd, intel, nvidia' ;; esac
if [[ "$MODE" == native && "$VIDEO_ACCEL" != none ]]; then
  bash "$ROOT/deploy/boosty-video-deps" "$VIDEO_ACCEL"
fi

# Do not rotate an existing certificate/key pair on ordinary updates.
cert="$ETC/tls/fullchain.pem"
key="$ETC/tls/privkey.pem"
if [[ -s "$cert" && -s "$key" ]]; then
  say 'Существующие сертификаты HTTPS сохранены.'
elif [[ -e "$cert" || -e "$key" ]]; then
  die 'Найдена только часть TLS-пары. Восстановите сертификат и ключ из резервной копии'
else
  TLS_MODE="${BD_TLS_MODE:-}"
  if [[ -z "$TLS_MODE" ]]; then
    say 'HTTPS: 1) самоподписанный; 2) существующие PEM; 3) Let’s Encrypt (домен, TCP 80)'
    [[ -r /dev/tty ]] || die 'Укажите BD_TLS_MODE=selfsigned|existing|letsencrypt'
    read -r -p 'Выбор [1]: ' choice </dev/tty
    case "${choice:-1}" in 1) TLS_MODE=selfsigned ;; 2) TLS_MODE=existing ;; 3) TLS_MODE=letsencrypt ;; *) die 'Неверный HTTPS-режим' ;; esac
  fi
  case "$TLS_MODE" in
    selfsigned)
      if [[ "$PUBLIC_HOST" == *:* || "$PUBLIC_HOST" =~ ^[0-9.]+$ ]]; then san="IP:$PUBLIC_HOST"
      else san="DNS:$PUBLIC_HOST"; fi
      openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 365 \
        -subj '/CN=Boosty Downloader' -addext "subjectAltName=$san" \
        -keyout "$key" -out "$cert" >/dev/null 2>&1 ||
        die 'Не удалось создать самоподписанный сертификат'
      say 'HTTPS: самоподписанный сертификат (предупреждение браузера ожидаемо).'
      ;;
    existing)
      crt="${BD_EXISTING_CERT:-}"
      pemkey="${BD_EXISTING_KEY:-}"
      if [[ -z "$crt" ]]; then crt="$(ask 'Путь к сертификату PEM' '')"; fi
      if [[ -z "$pemkey" ]]; then pemkey="$(ask 'Путь к ключу PEM' '')"; fi
      [[ -f "$crt" && -f "$pemkey" ]] || die 'Нет указанных файлов PEM'
      openssl x509 -in "$crt" -noout >/dev/null 2>&1 || die 'Недопустимый сертификат PEM'
      openssl pkey -in "$pemkey" -noout >/dev/null 2>&1 || die 'Недопустимый TLS-ключ PEM'
      certpub="$(openssl x509 -in "$crt" -pubkey -noout | openssl pkey -pubin -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)"
      keypub="$(openssl pkey -in "$pemkey" -pubout -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)"
      [[ "$certpub" == "$keypub" ]] || die 'Сертификат и приватный ключ не совпадают'
      install -m 0640 -o root -g boosty-downloader "$crt" "$cert"
      install -m 0640 -o root -g boosty-downloader "$pemkey" "$key"
      ;;
    letsencrypt)
      [[ "$PUBLIC_HOST" == *.* && "$PUBLIC_HOST" != *:* && ! "$PUBLIC_HOST" =~ ^[0-9.]+$ ]] ||
        die 'Для Let’s Encrypt необходим домен, а не IP'
      require_command certbot certbot
      say 'Домен должен указывать на VPS, TCP 80 должен быть доступен.'
      certbot certonly --standalone --non-interactive --agree-tos \
        --register-unsafely-without-email --cert-name boosty-downloader \
        --config-dir "$ETC/acme" --work-dir "$DATA/acme-work" \
        --logs-dir "$DATA/acme-logs" -d "$PUBLIC_HOST" ||
        die 'Не удалось получить сертификат Let’s Encrypt'
      install -m 0640 -o root -g boosty-downloader \
        "$ETC/acme/live/boosty-downloader/fullchain.pem" "$cert"
      install -m 0640 -o root -g boosty-downloader \
        "$ETC/acme/live/boosty-downloader/privkey.pem" "$key"
      ;;
    *) die 'BD_TLS_MODE: selfsigned, existing, letsencrypt' ;;
  esac
  chown root:boosty-downloader "$cert" "$key"
  chmod 0640 "$cert" "$key"
  printf '%s\n' "$TLS_MODE" > "$ETC/tls/mode"
  chown root:boosty-downloader "$ETC/tls/mode"
  chmod 0640 "$ETC/tls/mode"
fi

release_binary() {
  local arch name url tag expected actual edge_api
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;;
    *) die 'Native поддерживает x86_64 и aarch64' ;;
  esac
  name="boosty-downloader-linux-$arch"
  if [[ -n "${BD_NATIVE_BINARY:-}" ]]; then
    [[ -f "$BD_NATIVE_BINARY" && "${BD_NATIVE_SHA256:-}" =~ ^[a-fA-F0-9]{64}$ ]] ||
      die 'Для BD_NATIVE_BINARY требуется файл и BD_NATIVE_SHA256'
    install -m 0755 "$BD_NATIVE_BINARY" "$WORK/boosty-downloader"
    actual="$(sha256sum "$WORK/boosty-downloader" | cut -d' ' -f1)"
    [[ "$actual" == "${BD_NATIVE_SHA256,,}" ]] || die 'SHA-256 собственного бинарника не совпадает'
    return 0
  fi
  if [[ -n "${BD_RELEASE_TAG:-}" ]]; then
    tag="$BD_RELEASE_TAG"
    [[ "$tag" =~ ^(v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.-]+)?|edge-[a-f0-9]{12})$ ]] ||
      die 'BD_RELEASE_TAG: ожидается vX.Y.Z либо edge-<12-hex>'
    url="https://github.com/SawaMEN/boosty_downloader/releases/download/$tag"
    curl -fLsS --retry 2 --connect-timeout 12 --max-time 70 \
      "$url/SHA256SUMS" -o "$WORK/SHA256SUMS" || die "Нет манифеста контрольных сумм у релиза $tag"
  else
    # Pin both manifest and binary to one immutable release tag.
    tag="$(curl -fLsS --retry 2 --connect-timeout 12 --max-time 25 \
      'https://api.github.com/repos/SawaMEN/boosty_downloader/releases/latest' 2>/dev/null |
      python3 -c 'import json,sys;print(json.load(sys.stdin).get("tag_name",""))' 2>/dev/null || true)"
    if [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.-]+)?$ ]]; then
      url="https://github.com/SawaMEN/boosty_downloader/releases/download/$tag"
      curl -fLsS --retry 2 --connect-timeout 12 --max-time 70 \
        "$url/SHA256SUMS" -o "$WORK/SHA256SUMS" ||
        die "Не удалось скачать SHA256SUMS стабильного релиза $tag"
      say "Выбран стабильный релиз $tag."
    else
      # Stable not published yet. Preview follows successful main CI.
      edge_api="$WORK/releases.json"
      if ! curl -fLsS --retry 2 --connect-timeout 12 --max-time 30 \
        'https://api.github.com/repos/SawaMEN/boosty_downloader/releases?per_page=25' \
        -o "$edge_api"; then return 1; fi
      tag="$(python3 - "$edge_api" <<'PY'
import json,re,sys
with open(sys.argv[1],encoding='utf-8') as file:
    releases=json.load(file)
if not isinstance(releases,list):
    sys.exit(1)
for item in releases:
    tag=item.get('tag_name','')
    assets={asset.get('name') for asset in item.get('assets',[])}
    if (item.get('prerelease') and not item.get('draft')
        and re.fullmatch(r'edge-[0-9a-f]{12}',tag)
        and {'SHA256SUMS','boosty-downloader-linux-amd64',
             'boosty-downloader-linux-arm64'} <= assets):
        print(tag)
        break
PY
      )"
      [[ "$tag" =~ ^edge-[a-f0-9]{12}$ ]] || return 1
      url="https://github.com/SawaMEN/boosty_downloader/releases/download/$tag"
      curl -fLsS --retry 2 --connect-timeout 12 --max-time 70 \
        "$url/SHA256SUMS" -o "$WORK/SHA256SUMS" ||
        die 'Не удалось скачать контрольные суммы preview-релиза'
      say "Стабильный релиз отсутствует; используется проверенная CI-сборка $tag (preview)."
    fi
  fi
  curl -fLsS --retry 2 --connect-timeout 12 --max-time 90 \
    "$url/$name" -o "$WORK/$name" || die 'Не удалось скачать бинарник релиза'
  expected="$(awk -v f="$name" '$2==f {print $1}' "$WORK/SHA256SUMS")"
  actual="$(sha256sum "$WORK/$name" | cut -d ' ' -f1)"
  [[ "$expected" =~ ^[a-f0-9]{64}$ && "$actual" == "$expected" ]] ||
    die 'Контрольная сумма релиза не совпала! Отмена установки.'
  install -m 0755 "$WORK/$name" "$WORK/boosty-downloader"
  # Ship the matching database inspection utility, not a stale helper.
  local helper="boosty-dbcheck-linux-$arch" helper_expected helper_actual
  curl -fLsS --retry 2 --connect-timeout 12 --max-time 90 \
    "$url/$helper" -o "$WORK/$helper" || die 'Нет утилиты boosty-dbcheck в релизе'
  helper_expected="$(awk -v f="$helper" '$2==f {print $1}' "$WORK/SHA256SUMS")"
  helper_actual="$(sha256sum "$WORK/$helper" | cut -d ' ' -f1)"
  [[ "$helper_expected" =~ ^[a-f0-9]{64}$ && "$helper_actual" == "$helper_expected" ]] ||
    die 'SHA-256 boosty-dbcheck не совпала'
  install -m 0755 "$WORK/$helper" "$WORK/boosty-dbcheck"
  RELEASE_ID="$tag"
}

go_binary() {
  local existing='' arch json version filename expected actual dest
  if command -v go >/dev/null; then
    existing="$(command -v go)"
    if "$existing" version | grep -Eq 'go1\.(2[3-9]|[3-9][0-9])\.'; then
      printf '%s' "$existing"
      return 0
    fi
  fi
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;;
    *) die 'Для сборки требуется amd64 или arm64' ;;
  esac
  say 'Загрузка актуального стабильного Go с go.dev (SHA-256 обязателен)...' >&2
  json="$WORK/go-versions.json"
  curl -fLsS --retry 3 --connect-timeout 12 \
    'https://go.dev/dl/?mode=json' -o "$json" ||
    die 'Не удалось определить версию Go; установите Go >= 1.23 вручную'
  read -r version filename expected < <(python3 - "$json" "$arch" <<'PY'
import json,sys
data=json.load(open(sys.argv[1],encoding='utf-8'))
for release in data:
    if not release.get('stable'): continue
    for item in release.get('files',[]):
        if item.get('os')=='linux' and item.get('arch')==sys.argv[2] and item.get('kind')=='archive':
            print(release['version'],item['filename'],item['sha256'])
            sys.exit(0)
sys.exit(1)
PY
  )
  [[ "$version" =~ ^go1\.(2[3-9]|[3-9][0-9])\.[0-9]+$ &&
     "$filename" =~ ^go[0-9.]+\.linux-(amd64|arm64)\.tar\.gz$ &&
     "$expected" =~ ^[a-f0-9]{64}$ ]] ||
    die 'Невозможно проверить архив Go'
  dest="/opt/boosty-downloader-go/$version"
  if [[ ! -x "$dest/go/bin/go" ]]; then
    [[ ! -e "$dest" ]] || die 'На диске имеется повреждённый каталог Go; удалите его вручную после проверки'
    mkdir -p /opt/boosty-downloader-go
    curl -fLsS --retry 3 --connect-timeout 12 \
      "https://go.dev/dl/$filename" -o "$WORK/go.tar.gz" ||
      die 'Не удалось загрузить архив Go'
    actual="$(sha256sum "$WORK/go.tar.gz" | cut -d ' ' -f1)"
    [[ "$actual" == "$expected" ]] || die 'SHA-256 официального Go не совпадает'
    local stage="$WORK/go-extract"
    mkdir -p "$stage"
    tar -xzf "$WORK/go.tar.gz" -C "$stage"
    [[ -x "$stage/go/bin/go" ]] || die 'Архив Go повреждён'
    mv "$stage" "$dest"
  fi
  printf '%s' "$dest/go/bin/go"
}

health() {
  local n
  for n in $(seq 1 30); do
    if curl -kfsS --connect-timeout 2 --max-time 3 \
      "https://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  return 1
}

compose() {
  local -a files=(-f "$PROJECT/compose.yaml")
  case "$VIDEO_ACCEL" in
    amd|intel) files+=(-f "$PROJECT/compose.gpu-amd.yaml") ;;
    nvidia) files+=(-f "$PROJECT/compose.gpu-nvidia.yaml") ;;
  esac
  docker compose --project-name boosty-downloader --project-directory "$PROJECT" "${files[@]}" "$@"
}

native_install() {
  if [[ "${BD_BUILD_FROM_SOURCE:-0}" == 1 ]]; then
    say 'Сборка из исходников...'
    local go; go="$(go_binary)"
    (cd "$ROOT" && CGO_ENABLED=0 "$go" build -trimpath -ldflags='-s -w' \
      -o "$WORK/boosty-downloader" ./cmd/boosty-downloader &&
      CGO_ENABLED=0 "$go" build -trimpath -ldflags='-s -w' \
      -o "$WORK/boosty-dbcheck" ./cmd/boosty-dbcheck)
  elif ! release_binary; then
    say 'Опубликованный релиз не найден; сборка из исходников...'
    local go; go="$(go_binary)"
    (cd "$ROOT" && CGO_ENABLED=0 "$go" build -trimpath -ldflags='-s -w' \
      -o "$WORK/boosty-downloader" ./cmd/boosty-downloader &&
      CGO_ENABLED=0 "$go" build -trimpath -ldflags='-s -w' \
      -o "$WORK/boosty-dbcheck" ./cmd/boosty-dbcheck)
  else
    say 'Бинарник релиза загружен и проверен по SHA-256.'
  fi
  [[ -s "$WORK/boosty-downloader" ]] || die 'Бинарник не создан'
  local previous=''
  if [[ -f "$BIN" ]]; then
    cp -p "$BIN" "$WORK/old-binary"
    previous="$WORK/old-binary"
  fi
  install -d -m 0755 /usr/local/boosty-downloader
  install -m 0755 "$WORK/boosty-downloader" "$BIN.new"
  if [[ "$oldmode" == docker && -f "$PROJECT/compose.yaml" ]]; then
    compose down || die 'Не удалось остановить прежний Docker-контейнер'
  fi
  mv -f "$BIN.new" "$BIN"
  install -m 0644 "$ROOT/deploy/systemd/boosty-downloader.service" "$SERVICE"
  systemctl daemon-reload
  if ! systemctl enable boosty-downloader.service ||
     ! systemctl restart boosty-downloader.service ||
     ! health; then
    say 'Новая версия не прошла проверку. Восстанавливаем прежнюю...' >&2
    systemctl stop boosty-downloader.service >/dev/null 2>&1 || true
    if [[ -n "$previous" ]]; then
      install -m 0755 "$previous" "$BIN.new"
      mv -f "$BIN.new" "$BIN"
      systemctl restart boosty-downloader.service || true
    elif [[ "$oldmode" == docker ]]; then
      systemctl disable boosty-downloader.service >/dev/null 2>&1 || true
      compose up -d || true
    fi
    die 'Установка native не завершена, прежние данные сохранены'
  fi
  if [[ -n "$previous" ]]; then cp -p "$previous" "$BIN.prev"; fi
  if [[ -s "$WORK/boosty-dbcheck" ]]; then
    install -m 0755 "$WORK/boosty-dbcheck" /usr/local/bin/boosty-dbcheck
  fi
}

docker_install() {
  STAGE="$(mktemp -d /opt/.boosty-install-XXXXXXXX)"
  install -d -m 0755 "$STAGE/deploy/docker"
  cp -a "$ROOT/go.mod" "$ROOT/go.sum" "$ROOT/cmd" "$ROOT/internal" "$STAGE/"
  cp "$ROOT/deploy/docker/"*.yaml "$ROOT/deploy/docker/Dockerfile" "$STAGE/deploy/docker/"
  cp "$ROOT/deploy/docker/compose.yaml" "$STAGE/compose.yaml"
  cp "$ROOT/deploy/docker/compose.gpu-amd.yaml" "$STAGE/compose.gpu-amd.yaml"
  cp "$ROOT/deploy/docker/compose.gpu-nvidia.yaml" "$STAGE/compose.gpu-nvidia.yaml"
  local render_gid=0
  if [[ "$VIDEO_ACCEL" == amd || "$VIDEO_ACCEL" == intel ]]; then
    [[ -c /dev/dri/renderD128 ]] || die 'Нет /dev/dri/renderD128 для VA-API'
    render_gid="$(stat -c %g /dev/dri/renderD128)"
  fi
  local publish_ip='0.0.0.0'
  [[ "$PUBLIC_HOST" == 127.0.0.1 || "$PUBLIC_HOST" == ::1 ]] && publish_ip='127.0.0.1'
  printf 'BD_UID=%s\nBD_GID=%s\nBD_PORT=%s\nBD_PUBLISH_IP=%s\nBD_RENDER_GID=%s\n' \
    "$(id -u boosty-downloader)" "$(id -g boosty-downloader)" "$PORT" "$publish_ip" "$render_gid" \
    > "$STAGE/.env"
  chmod 0600 "$STAGE/.env"
  say 'Проверка новой конфигурации Compose до остановки службы...'
  local -a stagefiles=(-f "$STAGE/compose.yaml")
  case "$VIDEO_ACCEL" in
    amd|intel) stagefiles+=(-f "$STAGE/compose.gpu-amd.yaml") ;;
    nvidia) stagefiles+=(-f "$STAGE/compose.gpu-nvidia.yaml") ;;
  esac
  docker compose --project-name boosty-downloader --project-directory "$STAGE" "${stagefiles[@]}" config --quiet ||
    die 'Новая конфигурация Docker Compose некорректна'
  say 'Сборка Docker-кандидата без остановки рабочей версии...'
  docker build -f "$STAGE/deploy/docker/Dockerfile" -t boosty-downloader:install-candidate "$STAGE"
  if docker image inspect boosty-downloader:local >/dev/null 2>&1; then
    docker tag boosty-downloader:local boosty-downloader:install-rollback
    RESTORE_IMAGE=1
  fi
  local had_project=0
  if [[ -e "$PROJECT" ]]; then
    [[ -d "$PROJECT" && ! -L "$PROJECT" ]] || die 'Каталог Docker не является обычной папкой'
    BACKUP_PROJECT="$(mktemp -d /opt/.boosty-previous-XXXXXXXX)"
    rmdir "$BACKUP_PROJECT"
    had_project=1
    mv -T "$PROJECT" "$BACKUP_PROJECT"
  fi
  if ! mv -T "$STAGE" "$PROJECT"; then
    if ((had_project)); then mv -T "$BACKUP_PROJECT" "$PROJECT" || true; fi
    die 'Не удалось заменить каталог Docker; прежний каталог сохранён'
  fi
  STAGE=''
  docker tag boosty-downloader:install-candidate boosty-downloader:local ||
    die 'Не удалось включить новый Docker-образ'
  if [[ "$oldmode" == native ]]; then systemctl stop boosty-downloader.service; fi
  if ! compose up -d --force-recreate || ! health; then
    say 'Docker не прошёл проверку. Восстанавливаем прежнюю версию...' >&2
    compose down >/dev/null 2>&1 || true
    if ((RESTORE_IMAGE)); then
      docker tag boosty-downloader:install-rollback boosty-downloader:local
    fi
    if ((had_project)); then
      rm -rf -- "$PROJECT"
      mv -T "$BACKUP_PROJECT" "$PROJECT"
    fi
    if [[ "$oldmode" == native ]]; then
      systemctl start boosty-downloader.service || true
    elif [[ "$oldmode" == docker && $had_project -eq 1 ]]; then
      compose up -d --force-recreate || true
    fi
    die 'Установка Docker не завершена, прежние данные сохранены'
  fi
  if [[ "$oldmode" == native ]]; then
    systemctl disable boosty-downloader.service >/dev/null 2>&1 || true
  fi
  if ((had_project)); then rm -rf -- "$BACKUP_PROJECT"; fi
}

install -m 0755 "$ROOT/deploy/boosty-downloader-manager" /usr/local/bin/bdy
if [[ "$MODE" == native ]]; then native_install; else docker_install; fi
printf '%s\n' "$VIDEO_ACCEL" > "$ETC/video-mode"
printf '%s\n' "$MODE" > "$ETC/mode"
if [[ "$MODE" == native && -n "$RELEASE_ID" ]]; then
  printf '%s\n' "$RELEASE_ID" > "$ETC/installed-release"
  chmod 0640 "$ETC/installed-release"
else
  # A local source build is not necessarily an immutable published release.
  rm -f -- "$ETC/installed-release"
fi
chown root:boosty-downloader "$ETC/video-mode" "$ETC/mode"
chmod 0640 "$ETC/video-mode" "$ETC/mode"

if [[ "$(cat "$ETC/tls/mode" 2>/dev/null || echo '')" == letsencrypt ]]; then
  install -m 0755 "$ROOT/deploy/boosty-downloader-cert-renew" /usr/local/bin/bdy-cert-renew
  install -m 0644 "$ROOT/deploy/systemd/boosty-downloader-cert-renew.service" /etc/systemd/system/
  install -m 0644 "$ROOT/deploy/systemd/boosty-downloader-cert-renew.timer" /etc/systemd/system/
  systemctl daemon-reload
  systemctl enable --now boosty-downloader-cert-renew.timer
fi
install -m 0755 "$ROOT/deploy/boosty-auto-update" /usr/local/bin/bdy-auto-update
install -m 0644 "$ROOT/deploy/systemd/boosty-downloader-update.service" /etc/systemd/system/
install -m 0644 "$ROOT/deploy/systemd/boosty-downloader-update.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now boosty-downloader-update.timer ||
  say 'Предупреждение: таймер проверки обновлений не запущен.'

display="$PUBLIC_HOST"
[[ "$display" == *:* ]] && display="[$display]"
say ''
say "Установлен режим: $MODE"
say "Панель: https://$display:$PORT/"
say 'Управление: sudo bdy'
say 'Данные и настройки обновление не удаляет.'
