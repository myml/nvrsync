# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first so source edits do not invalidate the module cache.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/nvrsync ./cmd/nvrsync

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata ffmpeg \
 && adduser -D -H -u 1000 nvrsync
COPY --from=build /out/nvrsync /usr/local/bin/nvrsync

USER nvrsync
VOLUME ["/data"]
EXPOSE 8099

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8099/api/health >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/nvrsync"]
CMD ["-listen", ":8099", "-data", "/data"]
