#!/bin/bash
# Build Pawel's HolyArrs (branch pawel-build) and run it on the NAS.
# The binary is volume-mounted into the holyarrs service of
# /volume2/docker/media/compose.yaml (see compose-holyarrs.yaml here), so a
# deploy is: build, copy, restart that one container.
#   pawel/deploy-nas.sh            build + deploy
#   pawel/deploy-nas.sh --no-build deploy the last build again
set -euo pipefail
cd "$(dirname "$0")/.."
NAS=magician@primary
DIR=/volume2/docker/media/holyarrs
VER="pawel-$(git rev-parse --short HEAD)$(git diff --quiet || echo -dirty)"
if [ "${1:-}" != "--no-build" ]; then
  (cd app/frontend && npm ci --no-audit --no-fund >/dev/null && npm run build >/dev/null)
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VER" \
    -o bin/holyarrs-nas ./app/backend/cmd/holyarrs
fi
scp -q bin/holyarrs-nas "$NAS:/tmp/holyarrs.new"
ssh "$NAS" "sudo install -o 1034 -g 100 -m 755 /tmp/holyarrs.new $DIR/bin/holyarrs && rm /tmp/holyarrs.new \
  && cd /volume2/docker/media && sudo /usr/local/bin/docker compose up -d holyarrs >/dev/null && sudo /usr/local/bin/docker restart holyarrs >/dev/null"
for i in $(seq 1 60); do
  if ssh "$NAS" 'curl -sf -o /dev/null http://127.0.0.1:6262/login'; then echo "deployed $VER"; exit 0; fi
  sleep 2
done
echo "holyarrs did not answer on :6262" >&2; exit 1
