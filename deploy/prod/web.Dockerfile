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

FROM caddy:2.10-alpine
# Binary caddy resmi membawa file capability cap_net_bind_service; dengan cap_drop ALL + no-new-privileges
# exec-nya ditolak ("operation not permitted"). Port 8080 tidak butuh capability → dicabut.
RUN apk add --no-cache libcap && setcap -r /usr/bin/caddy && apk del libcap
COPY deploy/prod/web.Caddyfile /etc/caddy/Caddyfile
COPY --from=web /web/dist /srv/payrun
USER 65534:65534
EXPOSE 8080
