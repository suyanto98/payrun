#!/usr/bin/env bash
# Membangun image rilis PayRun untuk satu SHA (dipakai CI main.yml dan uji lokal):
#   scripts/build-images.sh <prefix> <sha> [--push]      contoh prefix: ghcr.io/<owner>/
# Nama sama dengan deploy/prod/compose.prod.yml: ${prefix}payrun-{app,web,backup}:<sha>.
set -euo pipefail
cd "$(dirname "$0")/.."
PREFIX=${1?usage: build-images.sh <prefix> <sha> [--push]}
SHA=${2:?sha}
PUSH=${3:-}
build() { # <name> <dockerfile> [extra args...]
  local name=$1 file=$2; shift 2
  docker build -q -f "$file" -t "${PREFIX}payrun-$name:$SHA" --label "org.opencontainers.image.revision=$SHA" "$@" . >/dev/null
  echo "built ${PREFIX}payrun-$name:$SHA"
  if [ "$PUSH" = --push ]; then docker push -q "${PREFIX}payrun-$name:$SHA" >/dev/null && echo "pushed ${PREFIX}payrun-$name:$SHA"; fi
}
# Image yang di-deploy ke VM demo publik = varian demo (endpoint /demo/* + titik fault + migrasi demo).
# Endpoint demo tetap mati kecuali DEMO_MODE=true + DEMO_TOKEN (27 §9.1).
build app deploy/Dockerfile --target payrun --build-arg "VERSION=$SHA" --build-arg BUILD_TAGS=demo
build web deploy/prod/web.Dockerfile
build backup deploy/backup/Dockerfile
