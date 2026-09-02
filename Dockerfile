# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.24 AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILT_AT=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/oss/oss-server/internal/version.Version=$VERSION -X github.com/oss/oss-server/internal/version.Commit=$COMMIT -X github.com/oss/oss-server/internal/version.BuiltAt=$BUILT_AT" \
      -o /out/oss-server ./cmd/server

FROM alpine:3.24

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 oss \
    && adduser -S -D -H -u 10001 -G oss oss \
    && mkdir -p /app/data /app/runtime \
    && chown oss:oss /app/data /app/runtime

WORKDIR /app

COPY --from=build --chown=oss:oss --chmod=755 /out/oss-server /app/runtime/oss-server
COPY configs /app/configs

ENV OSS_ENV=prod \
    OSS_SERVER_HOST=0.0.0.0 \
    OSS_SERVER_PORT=8080 \
    OSS_STORAGE_DIR=/app/data \
    OSS_DB_DRIVER=sqlite \
    OSS_DB_DSN=/app/data/oss.db

USER 10001:10001

EXPOSE 8080

ENTRYPOINT ["/app/runtime/oss-server"]
