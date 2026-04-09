#!/bin/sh
set -e

echo "==> Starting marketflow-ai..."
echo "    APP_ENV=${APP_ENV:-development}"
echo "    HTTP_PORT=${HTTP_PORT:-4001}"

exec /usr/local/bin/marketflow-ai
