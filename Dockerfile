# ---- build stage -----------------------------------------------------------
FROM golang:1.23-alpine AS build

WORKDIR /src

# Module cache is populated separately so source edits don't re-download deps.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/nexus ./cmd/gateway

# ---- runtime stage ---------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -S gateway \
 && adduser -S -G gateway -H -s /sbin/nologin gateway

COPY --from=build /out/nexus /usr/local/bin/nexus
COPY migrations /opt/gateway/migrations

USER gateway
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/usr/local/bin/nexus"]
