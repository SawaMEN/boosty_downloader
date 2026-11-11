# Видео и аппаратное ускорение

Сервис сохраняет доступные прямые медиафайлы и поддерживает часть конечных HLS/DASH VOD. Отдельная обработка FFmpeg может создать дополнительную версию локального видео, **сохраняя исходник**. DRM, live-видео, сложные DASH/HLS и сторонние защищённые плееры не гарантируются.

## Режимы обработки

| Режим | Что требуется |
| --- | --- |
| `cpu` | FFmpeg; без GPU |
| `vaapi` | AMD/Linux, Mesa, `/dev/dri` и разрешения render |
| `qsv` | Intel с поддерживаемым Quick Sync и драйверами |
| `nvenc` | NVIDIA-драйвер и совместимый FFmpeg |
| `auto` | Автоматический выбор при доступности устройства |

В панели администратора: **Настройки → Видео → Диагностика**. Проверяйте фактическое тестовое кодирование, а не только список доступных энкодеров. Видеообработка по умолчанию не должна включаться без выбора администратора.

## Native

При установке или повторном запуске установщика задайте нужный backend:

```bash
sudo env BD_VIDEO_ACCEL=amd bash install.sh --mode native
# или BD_VIDEO_ACCEL=intel / nvidia
```

Скрипт `deploy/boosty-video-deps` помогает установить FFmpeg и инструменты для VA-API на поддерживаемых дистрибутивах. Драйверы NVIDIA устанавливаются отдельно. После изменения групп и драйверов перезапустите службу: `sudo bdy restart`.

## Docker

```bash
sudo env BD_VIDEO_ACCEL=amd bash install.sh --mode docker
# либо intel / nvidia
```

Для AMD/Intel установщик подключает `/dev/dri` через Compose overlay, для NVIDIA требуется [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html). Не запускайте контейнер с `privileged: true` и не подключайте Docker socket.

## Проверка

```bash
ffmpeg -hide_banner -encoders
ls -l /dev/dri/renderD* 2>/dev/null
nvidia-smi 2>/dev/null
sudo bdy logs
```

Доступность GPU зависит от драйвера, модели и версии FFmpeg. На VPS без GPU выбирайте CPU или отключите создание производных файлов. Перед массовой обработкой проверьте свободное место: оригинал и перекодированная копия могут занимать диск одновременно.