# Установка, обновления и откат

## Первая установка

```bash
curl -fsSL https://raw.githubusercontent.com/SawaMEN/boosty_downloader/main/install.sh | sudo bash
```

Скрипт запускается от root, сам получает исходники из публичного GitHub, предлагает Native (без Docker) или Docker и создаёт учётную запись администратора. Порт HTTPS по умолчанию `8443`; сертификат — самоподписанный, предоставленный PEM или Let's Encrypt на домене.

Установка без вопросов:

```bash
curl -fsSL https://raw.githubusercontent.com/SawaMEN/boosty_downloader/main/install.sh | \
  sudo BD_PUBLIC_HOST=203.0.113.10 BD_PORT=8443 BD_TLS_MODE=selfsigned bash -s -- --mode native
```

Замените IP. Для Let's Encrypt требуется доступный домен и открытый порт 80/TCP. Установщик не открывает порты в файрволле самостоятельно.

## Откуда берутся бинарники

1. **Stable:** GitHub Releases с тегом `vX.Y.Z`.
2. **Preview:** при отсутствии стабильного релиза берётся последняя предварительная сборка `edge-<12-hex>`, опубликованная **после успешного CI main**.
3. **Fallback:** если подходящего релиза нет, выполняется сборка из текущих исходников. При отсутствии Go установщик скачивает официальный архив Go и проверяет SHA-256.

Готовые бинарники `boosty-downloader-linux-amd64`, `boosty-downloader-linux-arm64`, `boosty-dbcheck-linux-*` публикуются вместе с `SHA256SUMS` в [GitHub Releases](https://github.com/SawaMEN/boosty_downloader/releases). В Native устанавливаются сервер и `boosty-dbcheck`; Docker собирает сервер в образе. Preview не является стабильным выпуском.

## Управление

```bash
sudo bdy status
sudo bdy logs
sudo bdy restart
sudo bdy reset-password
sudo bdy update          # проверить наличие обновления
sudo bdy update --apply  # установить вручную
sudo bdy rollback        # вернуть сохранённую предыдущую версию
```

В панели администратора задаются канал **stable** или **dev** (preview), интервал проверки от 1 до 168 часов и автоматическое применение. **Автоприменение выключено по умолчанию.** Таймер обновлений запускается на хосте от root, не из веб-процесса. Публичные релизы доступны **без gh и без GitHub-токена**.

## Обновление и восстановление

При повторном запуске `install.sh` сохраняются существующие конфигурация, TLS-ключи и медиа. Новая версия сначала подготавливается, затем проверяется по `/healthz`; если запуск не удался, установщик пытается вернуть прежний режим.

`sudo bdy rollback` восстанавливает сохранённый бинарник или Docker-образ, **не откатывает пользовательские данные**. Прежде чем обновлять систему или переключать режим установки, сохраните при остановленном сервере:

- `/etc/boosty-downloader/` — конфигурация, сертификаты, секретный ключ.
- `/var/lib/boosty-downloader/` — JSON, токены, архивы и история.

Если ошибка связана с данными или несовместимой схемой, нужен полный файловый бэкап, а не только предыдущий бинарник.

## Диагностика

```bash
sudo bdy status
sudo bdy logs
sudo systemctl status boosty-downloader-update.timer
sudo journalctl -u boosty-downloader-update.service -n 100
```

Для принудительной сборки Native из исходников: `BD_BUILD_FROM_SOURCE=1`. Для установки заданного тега: `BD_RELEASE_TAG=vX.Y.Z` либо `BD_RELEASE_TAG=edge-xxxxxxxxxxxx`. Не используйте публичные ссылки на непроверенные сторонние скрипты с `sudo`.