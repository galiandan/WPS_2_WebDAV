ARG GO_BUILDER_IMAGE=golang:1.25.0
FROM ${GO_BUILDER_IMAGE} AS builder

ARG VERSION=0.9.92
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

WORKDIR /src
COPY go /src/go
WORKDIR /src/go

RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -o /out/wps-adapter ./cmd/wps-adapter

FROM scratch

ARG APP_UID=1000
ARG APP_GID=1000

WORKDIR /app
COPY --from=builder /out/wps-adapter /usr/local/bin/wps-adapter
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

USER ${APP_UID}:${APP_GID}

ENTRYPOINT ["/usr/local/bin/wps-adapter", "serve"]
