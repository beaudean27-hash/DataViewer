# DaVi (Data Viewer) — ISAK container image
#
# Two build modes via --build-arg:
#
#   DEV (default — works on any laptop / CI without DoD network):
#     docker build -t davi:dev .
#
#   RELEASE (for Iron Bank submission / fielded ISAK):
#     docker build \
#       --build-arg BASE_IMAGE=registry1.dso.mil/ironbank/opensource/nginx/nginx \
#       --build-arg BASE_TAG=1.27.3 \
#       -t davi:0.1.0 .
#
# The image ships only static web assets. The nginx site config is provided
# at deploy time by the Helm chart via a ConfigMap mounted at
# /etc/nginx/conf.d/default.conf, so backend hostnames (Elasticsearch, tile
# server) are never baked in.
#
# Local smoke test (uses the base image's default :80 listener, no upstreams):
#   docker run --rm -p 8080:80 davi:dev
#   # then browse http://localhost:8080
#
# In-cluster the Helm ConfigMap overrides default.conf to listen on :8080
# (Iron Bank nginx runs non-root, can't bind :80).

ARG BASE_IMAGE=nginx
ARG BASE_TAG=1.27-alpine

FROM ${BASE_IMAGE}:${BASE_TAG}

# Iron Bank nginx runs as a non-root user (uid varies by tag). Web root is
# /usr/share/nginx/html and the default listen port is 8080.
COPY legacy/davi-v2/src/                /usr/share/nginx/html/
COPY legacy/davi-v2/vendor/Cesium/      /usr/share/nginx/html/Cesium/
COPY legacy/davi-v2/vendor/mvt/         /usr/share/nginx/html/mvt/
COPY legacy/davi-v2/vendor/milsymbol/   /usr/share/nginx/html/milsymbol/

EXPOSE 8080
