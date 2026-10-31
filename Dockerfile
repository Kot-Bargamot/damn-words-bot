# Прокси на этапе сборки (go mod download) передаётся стандартными build-args
# HTTP_PROXY / HTTPS_PROXY — их подставляет docker-compose.yml из PROXY_URL.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
# timetzdata — база часовых поясов внутри бинарника, apk в итоговом образе не нужен
RUN CGO_ENABLED=0 go build -trimpath -tags timetzdata -ldflags="-s -w" -o /engbot .

FROM alpine:3.20
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN adduser -D -u 10001 bot && mkdir -p /app/data && chown bot /app/data
COPY --from=build /engbot /app/engbot
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
USER bot
WORKDIR /app
VOLUME /app/data
ENV DB_PATH=/app/data/engbot.db
ENTRYPOINT ["/app/docker-entrypoint.sh"]
