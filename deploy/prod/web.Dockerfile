# syntax=docker/dockerfile:1
# Image web = build statis SPA + Caddy file server (port 8080, non-root). TLS dan header keamanan
# dipasang di edge bersama (Caddy LedgerPay, site payrun.caddy); container ini hanya di network `edge`.
# Context: root repo. web.Dockerfile.dockerignore menggantikan .dockerignore root (yang mengecualikan frontend/).
FROM node:22-bookworm-slim AS web
WORKDIR /web
COPY frontend/package.json frontend/yarn.lock ./
RUN --mount=type=cache,target=/usr/local/share/.cache/yarn yarn install --frozen-lockfile
COPY frontend/ .
# Tautan opsional di landing dan rangkuman demo (kosong = disembunyikan, tidak ada tautan rusak).
ARG VITE_REPO_URL="" VITE_CONTACT_URL="" VITE_VIDEO_URL=""
ENV VITE_REPO_URL=$VITE_REPO_URL VITE_CONTACT_URL=$VITE_CONTACT_URL VITE_VIDEO_URL=$VITE_VIDEO_URL
RUN yarn build

# Caddy dikompilasi ulang dengan golang.org/x/net yang sudah ditambal (rilis Caddy terbaru masih membawa
# versi dengan CVE HIGH di http2, ditemukan Trivy). Hapus stage ini begitu rilis resmi Caddy memuatnya.
FROM caddy:2.11.7-builder-alpine AS caddy
RUN xcaddy build v2.11.7 --replace golang.org/x/net=golang.org/x/net@v0.60.0

FROM caddy:2.11.7-alpine
# Binary hasil xcaddy tidak membawa file capability (binary resmi membawa cap_net_bind_service, yang
# ditolak exec-nya di bawah cap_drop ALL + no-new-privileges). Port 8080 tidak butuh capability.
COPY --from=caddy /usr/bin/caddy /usr/bin/caddy
COPY deploy/prod/web.Caddyfile /etc/caddy/Caddyfile
COPY --from=web /web/dist /srv/payrun
USER 65534:65534
EXPOSE 8080
