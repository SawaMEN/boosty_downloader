FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/boosty-downloader ./cmd/boosty-downloader

FROM alpine:3.22
RUN apk add --no-cache ca-certificates zstd ffmpeg libva mesa-va-gallium && mkdir -p /etc/boosty-downloader /var/lib/boosty-downloader
COPY --from=builder /out/boosty-downloader /usr/local/bin/boosty-downloader
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/boosty-downloader"]
