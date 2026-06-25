#!/usr/bin/env bash
# ----------------------------------------------------------------------------
# Build a self-contained DaVi ISAK deployment package.
#
# Output: dist/davi-isak-<tag>.tar.gz
#
# The package contains:
#   images/davi-<tag>.tar              docker save of the main app image
#   images/davi-discover-<tag>.tar     docker save of the discovery sidecar
#   chart/                             helm chart (copied from charts/davi/)
#   install.sh                         operator-facing installer (runs on ISAK)
#   README.md                          operator instructions
#   VERSION                            image refs + build date
#   SHA256SUMS                         file-level integrity manifest
#
# Usage:
#   scripts/build-isak-package.sh [tag]
#
# Examples:
#   scripts/build-isak-package.sh                  # tag defaults to v0.3.0-sidecar
#   scripts/build-isak-package.sh v0.3.1
#
# Env overrides:
#   APP_NAME       default: davi
#   SIDECAR_NAME   default: davi-discover
#   SIDECAR_TAG    default: same as the app tag arg
# ----------------------------------------------------------------------------
set -euo pipefail

APP_TAG="${1:-v0.6.0-tak-stream}"
APP_NAME="${APP_NAME:-davi}"
SIDECAR_NAME="${SIDECAR_NAME:-davi-discover}"
SIDECAR_TAG="${SIDECAR_TAG:-$APP_TAG}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

DIST="$REPO_ROOT/dist"
PKG_DIR="$DIST/davi-isak-${APP_TAG}"
PKG_TAR="$DIST/davi-isak-${APP_TAG}.tar.gz"

echo ">>> [1/7] preparing $PKG_DIR"
rm -rf "$PKG_DIR"
mkdir -p "$PKG_DIR/images" "$PKG_DIR/chart"

echo ">>> [2/7] building main image ${APP_NAME}:${APP_TAG}"
docker build -t "${APP_NAME}:${APP_TAG}" .

echo ">>> [3/7] building sidecar image ${SIDECAR_NAME}:${SIDECAR_TAG}"
docker build -f discover-sidecar/Dockerfile -t "${SIDECAR_NAME}:${SIDECAR_TAG}" discover-sidecar

echo ">>> [4/7] saving image tarballs"
docker save "${APP_NAME}:${APP_TAG}"          -o "$PKG_DIR/images/${APP_NAME}-${APP_TAG}.tar"
docker save "${SIDECAR_NAME}:${SIDECAR_TAG}"  -o "$PKG_DIR/images/${SIDECAR_NAME}-${SIDECAR_TAG}.tar"

echo ">>> [5/7] copying helm chart"
cp -r charts/davi/. "$PKG_DIR/chart/"

echo ">>> [6/7] writing install.sh + README + VERSION"
cat > "$PKG_DIR/VERSION" <<EOF
APP=${APP_NAME}:${APP_TAG}
SIDECAR=${SIDECAR_NAME}:${SIDECAR_TAG}
BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
BUILT_FROM=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
EOF

cat > "$PKG_DIR/install.sh" <<'INSTALL_EOF'
#!/usr/bin/env bash
# ----------------------------------------------------------------------------
# DaVi ISAK installer — runs on the ISAK K3s node as root.
#
# This script:
#   1. Imports the bundled docker image tarballs into k3s containerd
#   2. Detects the ISAK hostname/domain from /root/tacticalsetup/config/
#      (or accepts overrides via env vars)
#   3. Runs `helm upgrade --install davi` against the bundled chart
#   4. Waits for the rollout to complete
#
# Usage:
#   ./install.sh                            # full install with defaults
#   HOSTNAME_VAL=isak2 DOMAIN_VAL=army.mil ./install.sh
#   NAMESPACE=isak-davi RELEASE=davi ./install.sh
#   ES_HOST=elasticsearch.isak-data.svc.cluster.local ./install.sh
#   EXTRA_HELM_ARGS="--set backends.extras[0].name=foo ..." ./install.sh
#
# Env overrides:
#   NAMESPACE         default: isak-davi
#   RELEASE           default: davi
#   HOSTNAME_VAL      default: auto from isak_inputs.json
#   DOMAIN_VAL        default: auto from isak_inputs.json
#   ES_HOST           unset = nginx skips ES upstream
#   TILES_HOST        unset = nginx skips tiles upstream
#   ENABLE_SIDECAR    default: 1
#   EXTRA_HELM_ARGS   raw extra helm args (space-separated)
#   SKIP_IMPORT       set 1 to skip k3s ctr import (images already loaded)
#   SKIP_ROLLOUT      set 1 to skip kubectl rollout status wait
# ----------------------------------------------------------------------------
set -euo pipefail

PKG_DIR="$(cd "$(dirname "$0")" && pwd)"
NAMESPACE="${NAMESPACE:-isak-davi}"
RELEASE="${RELEASE:-davi}"
ENABLE_SIDECAR="${ENABLE_SIDECAR:-1}"

# Load VERSION
if [[ ! -r "$PKG_DIR/VERSION" ]]; then
  echo "ERROR: $PKG_DIR/VERSION missing — package is incomplete." >&2
  exit 1
fi
# shellcheck disable=SC1091
. "$PKG_DIR/VERSION"
: "${APP:?missing APP= line in VERSION}"
: "${SIDECAR:?missing SIDECAR= line in VERSION}"

APP_REPO="${APP%%:*}"
APP_TAG_="${APP##*:}"
SC_REPO="${SIDECAR%%:*}"
SC_TAG_="${SIDECAR##*:}"

# Sanity: required tools
need() { command -v "$1" >/dev/null 2>&1 || { echo "ERROR: '$1' not on PATH" >&2; exit 1; }; }
need k3s
need helm
need kubectl

# Auto-detect HOSTNAME / DOMAIN
detect_isak_identity() {
  local cfg="/root/tacticalsetup/config/isak_inputs.json"
  if [[ -r "$cfg" ]] && command -v jq >/dev/null 2>&1; then
    if [[ -z "${HOSTNAME_VAL:-}" ]]; then
      HOSTNAME_VAL="$(jq -r '.hostname // empty' "$cfg" 2>/dev/null || true)"
    fi
    if [[ -z "${DOMAIN_VAL:-}" ]]; then
      local fqdn
      fqdn="$(jq -r '.ingressFqdn // empty' "$cfg" 2>/dev/null || true)"
      # ingressFqdn looks like "public.isak2.army.mil" → strip "public." then "<hostname>."
      if [[ -n "$fqdn" && -n "${HOSTNAME_VAL:-}" ]]; then
        DOMAIN_VAL="${fqdn#public.}"
        DOMAIN_VAL="${DOMAIN_VAL#${HOSTNAME_VAL}.}"
      fi
    fi
  fi
  # Fall back to ingress_info.txt (FQDN only)
  if [[ -z "${HOSTNAME_VAL:-}" || -z "${DOMAIN_VAL:-}" ]]; then
    local info="/root/tacticalsetup/config/ingress_info.txt"
    if [[ -r "$info" ]]; then
      local line
      line="$(grep -Eo 'public\.[A-Za-z0-9._-]+' "$info" | head -1 || true)"
      if [[ -n "$line" ]]; then
        local rest="${line#public.}"
        [[ -z "${HOSTNAME_VAL:-}" ]] && HOSTNAME_VAL="${rest%%.*}"
        [[ -z "${DOMAIN_VAL:-}"   ]] && DOMAIN_VAL="${rest#*.}"
      fi
    fi
  fi
}
detect_isak_identity

if [[ -z "${HOSTNAME_VAL:-}" || -z "${DOMAIN_VAL:-}" ]]; then
  cat >&2 <<USAGE
ERROR: could not detect HOSTNAME/DOMAIN automatically.
       Set them and re-run, e.g.:
         HOSTNAME_VAL=isak2 DOMAIN_VAL=army.mil $0
       Or check /root/tacticalsetup/config/isak_inputs.json.
USAGE
  exit 1
fi

echo "=========================================================="
echo " DaVi ISAK install"
echo "   package      : $PKG_DIR"
echo "   release      : $RELEASE in namespace $NAMESPACE"
echo "   image        : $APP"
echo "   sidecar      : $SIDECAR (enabled=$ENABLE_SIDECAR)"
echo "   hostname     : $HOSTNAME_VAL"
echo "   domain       : $DOMAIN_VAL"
echo "   ingress fqdn : davi.public.${HOSTNAME_VAL}.${DOMAIN_VAL}"
echo "=========================================================="

# [1/4] Import images into k3s containerd
if [[ "${SKIP_IMPORT:-0}" != "1" ]]; then
  for tar in "$PKG_DIR"/images/*.tar; do
    echo "[1/4] importing $(basename "$tar")"
    k3s ctr images import "$tar"
  done
else
  echo "[1/4] SKIP_IMPORT=1 — skipping image import"
fi

# [2/4] Detect & adopt any orphan resources from non-Helm installs.
# Memory note from prior deploys: `kubectl apply` left a `<release>-davi-nginx`
# ConfigMap not owned by Helm; the upgrade refuses to adopt it.
for cm in "${RELEASE}-davi-nginx" "${RELEASE}-davi-discovery"; do
  if kubectl -n "$NAMESPACE" get cm "$cm" >/dev/null 2>&1; then
    owned="$(kubectl -n "$NAMESPACE" get cm "$cm" \
      -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)"
    if [[ -z "$owned" ]]; then
      echo "[2/4] adopting orphan ConfigMap $cm into release $RELEASE"
      kubectl -n "$NAMESPACE" annotate --overwrite cm "$cm" \
        "meta.helm.sh/release-name=$RELEASE" \
        "meta.helm.sh/release-namespace=$NAMESPACE"
      kubectl -n "$NAMESPACE" label --overwrite cm "$cm" \
        "app.kubernetes.io/managed-by=Helm"
    fi
  fi
done

# [3/4] Build helm args
HELM_ARGS=(
  upgrade --install "$RELEASE" "$PKG_DIR/chart"
  --namespace "$NAMESPACE" --create-namespace
  --set "image.repository=docker.io/library/$APP_REPO"
  --set "image.tag=$APP_TAG_"
  --set "image.pullPolicy=Never"
  --set "hostname=$HOSTNAME_VAL"
  --set "domain=$DOMAIN_VAL"
)

if [[ "$ENABLE_SIDECAR" == "1" ]]; then
  HELM_ARGS+=(
    --set "discovery.sidecar.enabled=true"
    --set "discovery.sidecar.image.repository=docker.io/library/$SC_REPO"
    --set "discovery.sidecar.image.tag=$SC_TAG_"
    --set "discovery.sidecar.image.pullPolicy=Never"
  )
fi

[[ -n "${ES_HOST:-}"    ]] && HELM_ARGS+=( --set "backends.elasticsearch.host=$ES_HOST" )
[[ -n "${TILES_HOST:-}" ]] && HELM_ARGS+=( --set "backends.tiles.host=$TILES_HOST" )

if [[ -n "${EXTRA_HELM_ARGS:-}" ]]; then
  # shellcheck disable=SC2206
  EXTRA_ARR=( $EXTRA_HELM_ARGS )
  HELM_ARGS+=( "${EXTRA_ARR[@]}" )
fi

echo "[3/4] running helm upgrade --install"
echo "      helm ${HELM_ARGS[*]}"
helm "${HELM_ARGS[@]}"

# [4/4] Wait for rollout
DEPLOY_NAME="${RELEASE}-davi"
if [[ "${SKIP_ROLLOUT:-0}" != "1" ]]; then
  echo "[4/4] waiting for rollout of deploy/$DEPLOY_NAME (2m timeout)"
  if ! kubectl -n "$NAMESPACE" rollout status "deploy/$DEPLOY_NAME" --timeout=2m; then
    echo
    echo "ERROR: rollout did not complete. Diagnostic snapshot:" >&2
    kubectl -n "$NAMESPACE" get pods -o wide >&2 || true
    kubectl -n "$NAMESPACE" describe "deploy/$DEPLOY_NAME" >&2 || true
    exit 1
  fi
else
  echo "[4/4] SKIP_ROLLOUT=1 — not waiting on rollout"
fi

echo
echo "=========================================================="
echo " OK. DaVi is up."
echo "   URL  : https://davi.public.${HOSTNAME_VAL}.${DOMAIN_VAL}/davi/"
echo "   Pods : kubectl -n $NAMESPACE get pods"
if [[ "$ENABLE_SIDECAR" == "1" ]]; then
  echo "   Disc : kubectl -n $NAMESPACE logs deploy/$DEPLOY_NAME -c discover --tail=30"
fi
echo "=========================================================="
INSTALL_EOF
chmod +x "$PKG_DIR/install.sh"

cat > "$PKG_DIR/README.md" <<README_EOF
# DaVi for ISAK — ${APP_TAG}

Self-contained installer for DaVi (Data Viewer) on an ISAK K3s node.

## What's inside

| Path | What |
|---|---|
| \`images/${APP_NAME}-${APP_TAG}.tar\` | Main DaVi web app (nginx + single-page Cesium UI) |
| \`images/${SIDECAR_NAME}-${SIDECAR_TAG}.tar\` | Active discovery sidecar (Option B) — watches K8s API for neighbouring \`isak-*\` Services and merges them into the static tools catalog |
| \`chart/\` | Helm chart |
| \`install.sh\` | Operator-facing installer — runs on the ISAK node |
| \`VERSION\` | Image tags + build provenance |
| \`SHA256SUMS\` | Integrity manifest |

## Prerequisites on the ISAK node

- Root shell (\`ssh root@<isak-host>\`)
- \`k3s\`, \`helm\`, \`kubectl\` on \`\$PATH\` (default on ISAK)
- \`/root/tacticalsetup/config/isak_inputs.json\` readable, or the
  hostname/domain supplied via env vars (see *Overrides* below)
- ~600 MB free disk for unpacking + image import

## Install

Copy the tarball to the ISAK node (any method — \`scp\`, USB, etc.):

\`\`\`bash
scp davi-isak-${APP_TAG}.tar.gz root@<isak-host>:/root/
\`\`\`

On the ISAK node:

\`\`\`bash
cd /root
tar xzf davi-isak-${APP_TAG}.tar.gz
cd davi-isak-${APP_TAG}
sha256sum -c SHA256SUMS         # integrity check (optional)
./install.sh
\`\`\`

The installer auto-detects \`HOSTNAME\` and \`DOMAIN\` from
\`/root/tacticalsetup/config/isak_inputs.json\`, imports both container
images into K3s' containerd, and runs \`helm upgrade --install davi\` with
the discovery sidecar enabled. The discovery sidecar queries the local K8s
API for Services in \`isak-*\` namespaces and merges any data-API endpoints
it recognises into DaVi's *Other ISAK Tools* catalog at runtime.

DaVi will be reachable at:

\`\`\`
https://davi.public.<hostname>.<domain>/davi/
\`\`\`

## Overrides

| Env var | Default | Purpose |
|---|---|---|
| \`NAMESPACE\` | \`isak-davi\` | Kubernetes namespace |
| \`RELEASE\` | \`davi\` | Helm release name |
| \`HOSTNAME_VAL\` | auto | Short hostname (e.g. \`isak2\`) |
| \`DOMAIN_VAL\` | auto | Base domain (e.g. \`army.mil\`) |
| \`ENABLE_SIDECAR\` | \`1\` | Set to \`0\` to skip the discovery sidecar |
| \`ES_HOST\` | unset | If a cluster Elasticsearch exists, set to its in-cluster Service FQDN. **Leave unset on clusters with no ES — nginx will crashloop on an unresolvable upstream.** |
| \`TILES_HOST\` | unset | Same as above, for a vector tile service |
| \`SKIP_IMPORT\` | unset | Skip \`k3s ctr images import\` (when both images already loaded) |
| \`SKIP_ROLLOUT\` | unset | Skip the \`kubectl rollout status\` wait |
| \`EXTRA_HELM_ARGS\` | empty | Raw extra args appended to the \`helm\` command, e.g. \`--set backends.extras[0].name=geoserver --set backends.extras[0].type=ogc --set backends.extras[0].service=geoserver.isak-gis.svc.cluster.local --set backends.extras[0].port=8080 --set backends.extras[0].basePath=/geoserver\` |

## Verifying after install

\`\`\`bash
kubectl -n isak-davi rollout status deploy/davi-davi
kubectl -n isak-davi get pods -o wide
kubectl -n isak-davi logs deploy/davi-davi -c discover --tail=30
kubectl -n isak-davi exec deploy/davi-davi -c davi -- \\
  curl -fsS http://127.0.0.1:9090/static.json | head -40
\`\`\`

Expected sidecar log lines:

- \`[discover] starting davi-discover; port=9090 refresh=1m0s …\`
- \`[discover] kube list services in N namespaces (filtered to M after include/exclude)\`
- \`[discover] refresh: probed M services, found K data-API endpoints\`

## Rollback

\`\`\`bash
helm rollback davi -n isak-davi
\`\`\`

## Uninstall

\`\`\`bash
helm uninstall davi -n isak-davi
kubectl delete namespace isak-davi
\`\`\`

## Provenance

Built from commit \`$(git rev-parse --short HEAD 2>/dev/null || echo unknown)\`,
tag \`${APP_TAG}\`, at \`$(date -u +%Y-%m-%dT%H:%M:%SZ)\`.
Source: <https://github.com/beaudean27-hash/DataViewer>.
README_EOF

echo ">>> [7/7] generating SHA256SUMS + tar.gz"
(
  cd "$PKG_DIR"
  find . -type f ! -name SHA256SUMS -print0 \
    | sort -z \
    | xargs -0 sha256sum > SHA256SUMS
)
tar -C "$DIST" -czf "$PKG_TAR" "davi-isak-${APP_TAG}"

echo
echo "=========================================================="
echo " package : $PKG_TAR"
echo "          ($(du -h "$PKG_TAR" | cut -f1))"
echo " contents:"
(cd "$PKG_DIR" && find . -type f -printf '   %P  (%s bytes)\n' | sort | sed -n '1,30p')
echo "=========================================================="
