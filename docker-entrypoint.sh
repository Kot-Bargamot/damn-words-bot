#!/bin/sh
# Опциональный прокси для всего контейнера.
# PROXY_URL раскладывается в стандартные переменные, которые читают бот и любые
# другие процессы внутри контейнера. Если HTTPS_PROXY/HTTP_PROXY заданы явно — они главнее.
set -e
if [ -n "$PROXY_URL" ]; then
  : "${HTTP_PROXY:=$PROXY_URL}"
  : "${HTTPS_PROXY:=$PROXY_URL}"
  : "${ALL_PROXY:=$PROXY_URL}"
  export HTTP_PROXY HTTPS_PROXY ALL_PROXY
  export http_proxy="$HTTP_PROXY" https_proxy="$HTTPS_PROXY" all_proxy="$ALL_PROXY"
fi
: "${NO_PROXY:=localhost,127.0.0.1,::1}"
export NO_PROXY no_proxy="$NO_PROXY"
exec /app/engbot "$@"
