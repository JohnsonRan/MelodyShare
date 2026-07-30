FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/share .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates su-exec && adduser -D -u 1000 share
WORKDIR /app
COPY --from=build /out/share /app/share
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh
ENV SHARE_ADDR=:8080 SHARE_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
# starts as root to chown the bind-mounted data dir, then drops to user share
ENTRYPOINT ["/app/docker-entrypoint.sh"]
