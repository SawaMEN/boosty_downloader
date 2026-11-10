#!/usr/bin/env bash
# Installation from a checked-out repository. The repo is currently private.
set -Eeuo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ETC=/etc/boosty-downloader
DATA=/var/lib/boosty-downloader
PROJECT=/opt/boosty-downloader
SERVICE=/etc/systemd/system/boosty-downloader.service
CMD=/usr/local/bin/bdy
if [[ ${EUID:-$(id -u)} -ne 0 ]];then echo 'Запустите sudo bash install.sh' >&2;exit 1;fi
[[ "$(uname -s)" == Linux ]] || { echo 'Требуется Linux.' >&2;exit 1; }
[[ -f "$ROOT/go.mod" && -f "$ROOT/deploy/docker/compose.yaml" ]] || { echo 'Запускайте установку из исходного репозитория.' >&2;exit 1; }
MODE="${1:-}"
if [[ "$MODE" == --mode ]];then MODE="${2:-}";fi
if [[ "$MODE" != native && "$MODE" != docker ]];then
  echo 'Boosty Downloader — установка / обновление'
  echo '1) Нативная установка (systemd)'
  echo '2) Docker Compose'
  echo '0) Выход'
  read -r -p 'Выбор: ' option </dev/tty
  case "$option" in 1) MODE=native;;2) MODE=docker;;0) exit 0;;*) echo 'Неизвестный вариант';exit 1;;esac
fi
if [[ "$MODE" == native ]];then command -v systemctl >/dev/null || { echo 'Нужен systemd.' >&2;exit 1; };fi
if [[ "$MODE" == docker ]];then command -v docker >/dev/null || { echo 'Требуется Docker Engine с docker compose.' >&2;exit 1; };docker compose version >/dev/null || { echo 'Требуется Docker Compose v2.' >&2;exit 1; };fi
if [[ "$MODE" == native ]] && ! command -v zstd >/dev/null 2>&1; then
  echo 'Установка Zstandard для экономного хранения текста и метаданных...'
  if command -v apt-get >/dev/null 2>&1; then apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y zstd
  elif command -v dnf >/dev/null 2>&1; then dnf install -y zstd
  elif command -v pacman >/dev/null 2>&1; then pacman -S --noconfirm --needed zstd
  elif command -v zypper >/dev/null 2>&1; then zypper --non-interactive install zstd
  else echo 'Установите пакет zstd и повторите установку.' >&2; exit 1; fi
fi
if ! id boosty-downloader >/dev/null 2>&1;then command -v useradd >/dev/null || { echo 'Требуется useradd.' >&2;exit 1; };useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin boosty-downloader;fi
# Video components are optional. Explicit BD_VIDEO_ACCEL opt-in is required
# for the first installation; later installations retain the chosen backend.
VIDEO_ACCEL="${BD_VIDEO_ACCEL:-$(cat "$ETC/video-mode" 2>/dev/null || echo none)}"
case "$VIDEO_ACCEL" in none|amd|intel|nvidia) ;; *) echo 'BD_VIDEO_ACCEL: none, amd, intel или nvidia' >&2;exit 1;; esac
if [[ "$MODE" == native && "$VIDEO_ACCEL" != none ]];then
  bash "$ROOT/deploy/boosty-video-deps" "$VIDEO_ACCEL"
fi

install -d -m 0750 -o root -g boosty-downloader "$ETC"
install -d -m 0700 -o boosty-downloader -g boosty-downloader "$DATA"
install -d -m 0750 -o root -g boosty-downloader "$ETC/tls"
# Existing config is preserved across both backends.
if [[ ! -f "$ETC/environment" ]];then
  command -v openssl >/dev/null || { echo 'Для генерации ключей требуется openssl.' >&2;exit 1; }
  if [[ -n "${BD_PUBLIC_HOST:-}" ]];then PUBLIC_HOST="$BD_PUBLIC_HOST";else
    echo 'Адрес панели: 1) IP-адрес  2) Домен  3) Только локально / SSH-туннель'
    read -r -p 'Выбор [1]: ' hostmode </dev/tty; hostmode="${hostmode:-1}"
    case "$hostmode" in 1) read -r -p 'Публичный IP: ' PUBLIC_HOST </dev/tty;;2) read -r -p 'Домен (без https://): ' PUBLIC_HOST </dev/tty;;3) PUBLIC_HOST=127.0.0.1;;*) echo 'Неверный выбор';exit 1;;esac
  fi
  [[ -n "$PUBLIC_HOST" && "$PUBLIC_HOST" =~ ^[a-zA-Z0-9.:-]+$ && "$PUBLIC_HOST" != -* ]] || { echo 'Недопустимый IP/домен.' >&2;exit 1; }
  PORT="${BD_PORT:-8443}";if [[ -z "${BD_PORT:-}" ]];then read -r -p 'Порт панели [8443]: ' answer </dev/tty;PORT="${answer:-8443}";fi
  [[ "$PORT" =~ ^[0-9]{1,5}$ ]] && ((10#$PORT >=1024 && 10#$PORT <=65535)) || { echo 'Порт 1024–65535.' >&2;exit 1; }
  USERNAME="${BD_ADMIN_USER:-admin}"
  [[ "$USERNAME" =~ ^[a-zA-Z0-9_.-]{1,32}$ ]] || { echo 'Недопустимый логин.' >&2;exit 1; }
  PASSWORD="${BD_ADMIN_PASSWORD:-$(openssl rand -hex 18)}"
  [[ ${#PASSWORD} -ge 12 && "$PASSWORD" =~ ^[a-zA-Z0-9_.-]+$ ]] || { echo 'Пароль: не менее 12 латинских символов/цифр.' >&2;exit 1; }
  SECRET="$(openssl rand -base64 32 | tr -d '\n')"
  cat > "$ETC/environment" <<ENV
BD_LISTEN="$(if [[ "$PUBLIC_HOST" == 127.0.0.1 ]]; then printf '127.0.0.1:%s' "$PORT"; else printf ':%s' "$PORT"; fi)"
BD_PORT="$PORT"
BD_DATA_DIR="$DATA"
BD_ADMIN_USER="$USERNAME"
BD_ADMIN_PASSWORD="$PASSWORD"
BD_SECRET_KEY="$SECRET"
BD_TLS_CERT="$ETC/tls/fullchain.pem"
BD_TLS_KEY="$ETC/tls/privkey.pem"
ENV
  chown root:boosty-downloader "$ETC/environment";chmod 0640 "$ETC/environment"
  printf '\nЛогин: %s\nПароль: %s\nСохраните пароль. Впоследствии он не выводится.\n' "$USERNAME" "$PASSWORD"
  printf '%s\n' "$PUBLIC_HOST" > "$ETC/public-host";chmod 0640 "$ETC/public-host"
else
  echo 'Существующая конфигурация сохранена.'
  # shellcheck disable=SC1091
  set -a;source "$ETC/environment";set +a
  PORT="$BD_PORT";PUBLIC_HOST="$(cat "$ETC/public-host" 2>/dev/null || echo 127.0.0.1)"
fi
if [[ ! -s "$ETC/tls/fullchain.pem" || ! -s "$ETC/tls/privkey.pem" ]];then
  TLS_MODE="${BD_TLS_MODE:-}"
  if [[ -z "$TLS_MODE" ]];then
    echo 'HTTPS: 1) Самоподписанный  2) Свои PEM  3) Let’s Encrypt (домен, порт 80)'
    read -r -p 'Выбор [1]: ' t </dev/tty
    case "${t:-1}" in 1) TLS_MODE=selfsigned;;2) TLS_MODE=existing;;3) TLS_MODE=letsencrypt;;*) echo 'Неверный выбор';exit 1;;esac
  fi
  if [[ "$TLS_MODE" == existing ]];then
    crt="${BD_EXISTING_CERT:-}";key="${BD_EXISTING_KEY:-}"
    if [[ -z "$crt" ]];then read -r -p 'Путь к fullchain.pem: ' crt </dev/tty;fi
    if [[ -z "$key" ]];then read -r -p 'Путь к privkey.pem: ' key </dev/tty;fi
    [[ -f "$crt" && -f "$key" ]] || { echo 'Указанные PEM-файлы отсутствуют';exit 1; }
    install -m 0640 -o root -g boosty-downloader "$crt" "$ETC/tls/fullchain.pem"
    install -m 0640 -o root -g boosty-downloader "$key" "$ETC/tls/privkey.pem"
    printf '%s\n' existing > "$ETC/tls/mode";chmod 0640 "$ETC/tls/mode"
  elif [[ "$TLS_MODE" == selfsigned ]];then
    command -v openssl >/dev/null || { echo 'Нужен openssl';exit 1; }
    san="DNS:$PUBLIC_HOST";if [[ "$PUBLIC_HOST" == 127.0.0.1 || "$PUBLIC_HOST" =~ ^[0-9.]+$ || "$PUBLIC_HOST" == *:* ]];then san="IP:$PUBLIC_HOST";fi
    openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 365 -subj '/CN=Boosty Downloader' -addext "subjectAltName=$san" -keyout "$ETC/tls/privkey.pem" -out "$ETC/tls/fullchain.pem" 2>/dev/null
    chown root:boosty-downloader "$ETC/tls/privkey.pem" "$ETC/tls/fullchain.pem"
    chmod 0640 "$ETC/tls/privkey.pem" "$ETC/tls/fullchain.pem"
    printf '%s\n' selfsigned > "$ETC/tls/mode";chmod 0640 "$ETC/tls/mode"
    echo 'Создан самоподписанный сертификат. Для доверенного HTTPS на домене используйте Let’s Encrypt.'
  elif [[ "$TLS_MODE" == letsencrypt ]];then
    [[ "$PUBLIC_HOST" =~ ^[a-zA-Z0-9.-]+$ && "$PUBLIC_HOST" == *.* && ! "$PUBLIC_HOST" =~ ^[0-9.]+$ ]] || { echo 'Let’s Encrypt здесь поддерживается только для домена.' >&2;exit 1; }
    if ! command -v certbot >/dev/null;then
      echo 'Для автоматического HTTPS нужен certbot. Устанавливаем системный пакет...'
      if command -v apt-get >/dev/null;then apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y certbot
      elif command -v dnf >/dev/null;then dnf install -y certbot
      elif command -v pacman >/dev/null;then pacman -S --noconfirm --needed certbot
      else echo 'Установите certbot и повторите команду.' >&2;exit 1;fi
    fi
    echo 'Для Let’s Encrypt домен должен указывать на VPS, TCP 80 должен быть доступен из интернета.'
    certbot certonly --standalone --non-interactive --agree-tos --register-unsafely-without-email \
      --cert-name boosty-downloader -d "$PUBLIC_HOST" --config-dir "$ETC/acme" --work-dir "$DATA/acme-work" --logs-dir "$DATA/acme-logs"
    install -m 0640 -o root -g boosty-downloader "$ETC/acme/live/boosty-downloader/fullchain.pem" "$ETC/tls/fullchain.pem"
    install -m 0640 -o root -g boosty-downloader "$ETC/acme/live/boosty-downloader/privkey.pem" "$ETC/tls/privkey.pem"
    printf '%s\n' letsencrypt > "$ETC/tls/mode";chmod 0640 "$ETC/tls/mode"
  else echo 'BD_TLS_MODE: selfsigned, existing или letsencrypt';exit 1;fi
fi
install -m 0755 "$ROOT/deploy/boosty-downloader-manager" "$CMD"
oldmode="$(cat "$ETC/mode" 2>/dev/null || echo none)"
download_release_binary() {
  local temp_dir="$1" arch
  case "$(uname -m)" in x86_64|amd64) arch=amd64;;aarch64|arm64) arch=arm64;;*) echo 'Архитектура не поддерживается';return 1;;esac
  if [[ -n "${BD_NATIVE_BINARY:-}" ]];then
    [[ -f "$BD_NATIVE_BINARY" ]] || { echo 'BD_NATIVE_BINARY не найден.' >&2;return 1; }
    [[ -n "${BD_NATIVE_SHA256:-}" ]] || { echo 'Для собственного бинарника обязательно укажите BD_NATIVE_SHA256.' >&2;return 1; }
    install -m 0755 "$BD_NATIVE_BINARY" "$temp_dir/boosty-downloader"
    [[ "$(sha256sum "$temp_dir/boosty-downloader" | cut -d ' ' -f 1)" == "$BD_NATIVE_SHA256" ]] || { echo 'SHA256 не совпадает.' >&2;return 1; }
    return 0
  fi
  if command -v gh >/dev/null 2>&1 && { [[ -n "${GH_TOKEN:-}" ]] || gh auth status >/dev/null 2>&1; };then
    local tag="${BD_RELEASE_TAG:-}" selector=()
    if [[ -n "$tag" ]];then selector=(--tag "$tag");fi
    if gh release download "${selector[@]}" --repo SawaMEN/boosty_downloader --dir "$temp_dir" --pattern "boosty-downloader-linux-$arch" --pattern SHA256SUMS --clobber;then
      if [[ -f "$temp_dir/SHA256SUMS" && -f "$temp_dir/boosty-downloader-linux-$arch" ]] &&
         (cd "$temp_dir" && sha256sum --check --ignore-missing SHA256SUMS | grep -Fq "boosty-downloader-linux-$arch: OK");then
        install -m 0755 "$temp_dir/boosty-downloader-linux-$arch" "$temp_dir/boosty-downloader"
        return 0
      fi
      echo 'Контрольная сумма релиза не подтверждена.' >&2
      return 2
    fi
    local run_id
    run_id="$(gh run list --repo SawaMEN/boosty_downloader --workflow ci.yml --branch main --status success --limit 1 --json databaseId --jq '.[0].databaseId' 2>/dev/null || true)"
    if [[ "$run_id" =~ ^[0-9]+$ ]];then
      if gh run download "$run_id" --repo SawaMEN/boosty_downloader --name "boosty-downloader-linux-$arch" --dir "$temp_dir";then
        if [[ -f "$temp_dir/SHA256SUMS" && -f "$temp_dir/boosty-downloader-linux-$arch" ]] &&
           (cd "$temp_dir" && sha256sum --check --ignore-missing SHA256SUMS | grep -Fq "boosty-downloader-linux-$arch: OK");then
          install -m 0755 "$temp_dir/boosty-downloader-linux-$arch" "$temp_dir/boosty-downloader"
          return 0
        fi
        echo 'Контрольная сумма CI-артефакта не подтверждена.' >&2
        return 2
      fi
    fi
  fi
  return 1
}
if [[ "$MODE" == native ]];then
  tmp="$(mktemp -d)";trap 'rm -rf -- "$tmp"' EXIT
  if [[ "${BD_BUILD_FROM_SOURCE:-0}" == 1 ]];then
    echo 'Локальная сборка из исходников...'
    command -v go >/dev/null || { echo 'Нужен Go >= 1.23.' >&2;exit 1; }
    (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$tmp/boosty-downloader" ./cmd/boosty-downloader)
  elif download_release_binary "$tmp";then
    echo 'Получен готовый бинарник, SHA256 проверен.'
  else
    if [[ -n "${BD_NATIVE_BINARY:-}" ]];then echo 'Локальный бинарник не прошёл проверку.' >&2;exit 1;fi
    echo 'Релиз недоступен или GitHub не авторизован; сборка из текущих исходников.'
    command -v go >/dev/null || { echo 'Нужен Go >= 1.23 либо gh с доступом к релизу GitHub.' >&2;exit 1; }
    (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$tmp/boosty-downloader" ./cmd/boosty-downloader)
  fi
  [[ -s "$tmp/boosty-downloader" ]] || { echo 'Сборка не выполнена';exit 1; }
  if [[ "$oldmode" == docker && -f "$PROJECT/compose.yaml" ]];then docker compose --project-directory "$PROJECT" -f "$PROJECT/compose.yaml" down;fi
  install -d -m 0755 /usr/local/boosty-downloader
  install -m 0755 "$tmp/boosty-downloader" /usr/local/boosty-downloader/boosty-downloader
  install -m 0644 "$ROOT/deploy/systemd/boosty-downloader.service" "$SERVICE"
  systemctl daemon-reload
  if ! systemctl enable --now boosty-downloader.service || ! systemctl restart boosty-downloader.service;then
    echo 'Служба не запустилась, проверьте: journalctl -u boosty-downloader -n 100' >&2
    if [[ "$oldmode" == docker ]];then docker compose --project-directory "$PROJECT" -f "$PROJECT/compose.yaml" up -d || true;fi
    exit 1
  fi
else
  echo 'Сборка Docker-образа...'
  install -d -m 0755 "$PROJECT"
  cp -a "$ROOT/go.mod" "$ROOT/cmd" "$ROOT/internal" "$PROJECT/"
  install -d -m 0755 "$PROJECT/deploy/docker"
  cp "$ROOT/deploy/docker/Dockerfile" "$PROJECT/deploy/docker/Dockerfile"
  cp "$ROOT/deploy/docker/compose.yaml" "$PROJECT/compose.yaml"
  cp "$ROOT/deploy/docker/compose.gpu-amd.yaml" "$PROJECT/compose.gpu-amd.yaml"
  cp "$ROOT/deploy/docker/compose.gpu-nvidia.yaml" "$PROJECT/compose.gpu-nvidia.yaml"
  compose_files=(-f "$PROJECT/compose.yaml")
  if [[ "$VIDEO_ACCEL" == amd || "$VIDEO_ACCEL" == intel ]];then
    [[ -c /dev/dri/renderD128 ]] || { echo 'Нет /dev/dri/renderD128. Настройте Mesa и GPU хоста.' >&2;exit 1; }
    compose_files+=(-f "$PROJECT/compose.gpu-amd.yaml")
    BD_RENDER_GID="$(stat -c '%g' /dev/dri/renderD128)"
    export BD_RENDER_GID
  elif [[ "$VIDEO_ACCEL" == nvidia ]];then
    compose_files+=(-f "$PROJECT/compose.gpu-nvidia.yaml")
  fi
  id_num="$(id -u boosty-downloader)";group_num="$(id -g boosty-downloader)"
  publish_ip="0.0.0.0";[[ "$PUBLIC_HOST" != 127.0.0.1 ]] || publish_ip="127.0.0.1"
  printf 'BD_UID=%s\nBD_GID=%s\nBD_PORT=%s\nBD_PUBLISH_IP=%s\nBD_RENDER_GID=%s\n' "$id_num" "$group_num" "$PORT" "$publish_ip" "${BD_RENDER_GID:-0}" > "$PROJECT/.env"
  chmod 0600 "$PROJECT/.env"
  docker compose --project-directory "$PROJECT" "${compose_files[@]}" build
  if [[ "$oldmode" == native ]];then systemctl stop boosty-downloader.service;fi
  if ! docker compose --project-directory "$PROJECT" "${compose_files[@]}" up -d;then
    echo 'Не удалось запустить Docker. Проверяйте docker compose logs.' >&2
    if [[ "$oldmode" == native ]];then systemctl start boosty-downloader.service || true;fi
    exit 1
  fi
  systemctl disable boosty-downloader.service >/dev/null 2>&1 || true
fi
printf '%s\n' "$VIDEO_ACCEL" > "$ETC/video-mode"
chmod 0640 "$ETC/video-mode"
printf '%s\n' "$MODE" > "$ETC/mode"
chmod 0640 "$ETC/mode"
if [[ -f "$ETC/tls/mode" && "$(cat "$ETC/tls/mode")" == letsencrypt ]];then
  if command -v systemctl >/dev/null;then
    install -m 0755 "$ROOT/deploy/boosty-downloader-cert-renew" /usr/local/bin/bdy-cert-renew
    install -m 0644 "$ROOT/deploy/systemd/boosty-downloader-cert-renew.service" /etc/systemd/system/boosty-downloader-cert-renew.service
    install -m 0644 "$ROOT/deploy/systemd/boosty-downloader-cert-renew.timer" /etc/systemd/system/boosty-downloader-cert-renew.timer
    systemctl daemon-reload
    systemctl enable --now boosty-downloader-cert-renew.timer
    echo 'Включено автоматическое продление сертификата.'
  else
    echo 'Для автоматического продления сертификата необходим systemd timer.'
  fi
fi
# The updater is a separate root-owned timer, never a privileged web endpoint.
if command -v systemctl >/dev/null && [[ -d /run/systemd/system ]]; then
  install -m 0755 "$ROOT/deploy/boosty-auto-update" /usr/local/bin/bdy-auto-update
  install -m 0644 "$ROOT/deploy/systemd/boosty-downloader-update.service" /etc/systemd/system/boosty-downloader-update.service
  install -m 0644 "$ROOT/deploy/systemd/boosty-downloader-update.timer" /etc/systemd/system/boosty-downloader-update.timer
  systemctl daemon-reload
  if ! systemctl enable --now boosty-downloader-update.timer; then
    echo "Автопроверку обновлений не удалось включить. Проверьте systemd timer." >&2
  fi
fi
echo "Установлен режим: $MODE"
echo "Панель: https://$PUBLIC_HOST:$PORT/"
echo 'Управление: sudo bdy'
