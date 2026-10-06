# syntax=docker/dockerfile:1
# Multi-stage: compile di image Go, jalankan di distroless (tanpa shell, tanpa package manager,
# user non-root). Permukaan serangan kecil (C-28). Satu binary untuk api/migrate/healthcheck
# (worker menyusul Sprint 4).

FROM golang:1.26.6-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
# BUILD_TAGS=demo → image demo (Sprint 8). Image release dibangun TANPA tag.
ARG BUILD_TAGS=""
# Tanpa `go mod download`: perintah itu mengunduh SEMUA modul di go.mod, termasuk dependency
# khusus test (testcontainers, Docker SDK) yang besar. `go build` hanya mengunduh yang dipakai binary.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -tags "${BUILD_TAGS}" -ldflags "-s -w" -o /out/ ./cmd/payrun

# ---- image aplikasi
FROM gcr.io/distroless/static-debian12:nonroot AS payrun
ARG VERSION=dev
LABEL org.opencontainers.image.title="payrun" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/payrun /app/payrun
USER nonroot:nonroot
ENV APP_VERSION=${VERSION}
EXPOSE 8080 9090
# distroless tidak punya curl: healthcheck memakai subcommand binary itu sendiri.
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=12 CMD ["/app/payrun", "healthcheck"]
ENTRYPOINT ["/app/payrun"]
CMD ["api"]
