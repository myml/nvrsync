FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/nvrsync ./cmd/nvrsync

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata ffmpeg && adduser -D -H -u 1000 nvrsync
COPY --from=build /out/nvrsync /usr/local/bin/nvrsync
USER nvrsync
VOLUME ["/data"]
EXPOSE 8099
ENTRYPOINT ["/usr/local/bin/nvrsync"]
CMD ["-listen", ":8099", "-data", "/data"]
