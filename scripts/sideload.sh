#!/usr/bin/env bash
# ----------------------------------------------------------------------------
# Build the DaVi image locally and sideload it onto a development ISAK node
# (which has no internet egress and is not yet wired to Iron Bank/Harbor).
#
# Flow:
#   1. docker build the image with the public nginx base (dev mode).
#   2. docker save to a tarball.
#   3. scp the tarball to the ISAK node.
#   4. ssh and `k3s ctr images import` so containerd can see the image.
#   5. (Optional) helm upgrade --install the chart in the isak-davi namespace.
#
# Usage:
#   scripts/sideload.sh <isak-host> [ssh-user] [image-tag]
#
# Examples:
#   scripts/sideload.sh isak2.army.mil
#   scripts/sideload.sh 10.0.0.42 root dev
#
# Env overrides:
#   IMAGE_NAME   default: davi
#   IMAGE_TAG    default: dev (also overridable as 3rd arg)
#   SSH_USER     default: root (also overridable as 2nd arg)
#   REMOTE_DIR   default: /tmp
#   DO_INSTALL   set to "1" to also run helm upgrade --install on the node
#   ES_HOST      passed to --set backends.elasticsearch.host when installing
#   TILES_HOST   passed to --set backends.tiles.host when installing
#   HOSTNAME_VAL passed to --set hostname (defaults to short host of ISAK_HOST)
#   DOMAIN_VAL   passed to --set domain (defaults to remainder of ISAK_HOST)
# ----------------------------------------------------------------------------
set -euo pipefail

ISAK_HOST="${1:-}"
SSH_USER="${2:-${SSH_USER:-root}}"
IMAGE_TAG="${3:-${IMAGE_TAG:-dev}}"
IMAGE_NAME="${IMAGE_NAME:-davi}"
REMOTE_DIR="${REMOTE_DIR:-/tmp}"

if [[ -z "$ISAK_HOST" ]]; then
  echo "usage: $0 <isak-host> [ssh-user] [image-tag]" >&2
  exit 2
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

IMAGE_REF="${IMAGE_NAME}:${IMAGE_TAG}"
TAR_LOCAL="${REPO_ROOT}/.zip_build/${IMAGE_NAME}-${IMAGE_TAG}.tar"
TAR_REMOTE="${REMOTE_DIR}/${IMAGE_NAME}-${IMAGE_TAG}.tar"

mkdir -p "$(dirname "$TAR_LOCAL")"

echo ">>> [1/4] building $IMAGE_REF (dev base = public nginx)"
docker build -t "$IMAGE_REF" .

echo ">>> [2/4] exporting to $TAR_LOCAL"
docker save "$IMAGE_REF" -o "$TAR_LOCAL"
echo "    $(du -h "$TAR_LOCAL" | cut -f1)"

echo ">>> [3/4] copying to ${SSH_USER}@${ISAK_HOST}:${TAR_REMOTE}"
scp "$TAR_LOCAL" "${SSH_USER}@${ISAK_HOST}:${TAR_REMOTE}"

echo ">>> [4/4] importing into k3s containerd"
ssh "${SSH_USER}@${ISAK_HOST}" \
  "k3s ctr images import '${TAR_REMOTE}' && k3s ctr images ls | grep '${IMAGE_NAME}'"

if [[ "${DO_INSTALL:-0}" == "1" ]]; then
  short_host="${HOSTNAME_VAL:-${ISAK_HOST%%.*}}"
  domain="${DOMAIN_VAL:-${ISAK_HOST#*.}}"
  if [[ "$short_host" == "$ISAK_HOST" ]]; then
    domain=""
  fi
  echo ">>> bonus: helm upgrade --install davi (host=$short_host domain=$domain)"

  # Sync the chart to the node, then install/upgrade from there.
  ssh "${SSH_USER}@${ISAK_HOST}" "mkdir -p ${REMOTE_DIR}/davi-chart"
  scp -r charts/davi/* "${SSH_USER}@${ISAK_HOST}:${REMOTE_DIR}/davi-chart/"

  install_cmd=(
    helm upgrade --install davi "${REMOTE_DIR}/davi-chart"
    --namespace isak-davi --create-namespace
    --set "image.repository=docker.io/library/${IMAGE_NAME}"
    --set "image.tag=${IMAGE_TAG}"
    --set "image.pullPolicy=Never"
    --set "hostname=${short_host}"
    --set "domain=${domain}"
  )
  [[ -n "${ES_HOST:-}"    ]] && install_cmd+=( --set "backends.elasticsearch.host=${ES_HOST}" )
  [[ -n "${TILES_HOST:-}" ]] && install_cmd+=( --set "backends.tiles.host=${TILES_HOST}" )

  ssh "${SSH_USER}@${ISAK_HOST}" "${install_cmd[*]}"
fi

echo ">>> done."
echo "    image on node : ${IMAGE_REF}"
echo "    chart values  : image.repository=docker.io/library/${IMAGE_NAME} image.tag=${IMAGE_TAG} image.pullPolicy=Never"
