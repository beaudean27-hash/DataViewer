# ─────────────────────────────────────────────────────────────────────────
# DaVi discovery sidecar.
#
# Stdlib-only Go binary that lists K8s Services in `isak-*` namespaces,
# probes each for known data-API signatures (ES, PostgREST, OGC, MinIO,
# TAK, REST), merges with a static catalog, and serves /static.json.
#
# Dev build (this repo, no Iron Bank):
#   docker build -f discover-sidecar/Dockerfile -t davi-discover:dev discover-sidecar
#
# Iron Bank build:
#   docker build \
#     --build-arg GO_IMAGE=registry1.dso.mil/ironbank/google/golang/golang-1.22 \
#     --build-arg RUN_IMAGE=registry1.dso.mil/ironbank/google/distroless/static \
#     -f discover-sidecar/Dockerfile -t registry1.dso.mil/ironbank/davi/davi-discover:0.1.0 discover-sidecar
# ─────────────────────────────────────────────────────────────────────────

ARG GO_IMAGE=golang:1.22-alpine
ARG RUN_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
# (no external deps, no go.sum)
COPY main.go ./
# Static, stripped, no DWARF, no cgo. Trimmed paths so the binary is reproducible.
RUN CGO_ENABLED=0 GOFLAGS="-trimpath" go build \
    -ldflags="-s -w -extldflags=-static" \
    -o /out/davi-discover ./...

FROM ${RUN_IMAGE}
COPY --from=build /out/davi-discover /usr/local/bin/davi-discover
USER 65532:65532
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/davi-discover"]
