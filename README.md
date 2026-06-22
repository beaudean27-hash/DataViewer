# DataViewer (DaVi) — ISAK packaging

DaVi is a Cesium-based tactical viewer. This repository now contains two
parallel deployment paths:

| Path | Status | Location |
|---|---|---|
| **ISAK container** (Iron Bank nginx + Helm + Traefik) | active | repo root |
| **Legacy RDA / JCRSD bootstrap** (`DaVi.zip`, `rda.manifest`) | preserved for reference / continued dev | [legacy/](legacy/) |

The web app sources of record live under [legacy/davi-v2/src](legacy/davi-v2/src);
both the legacy RDA build and the new ISAK image pull from there, so changes
to `index.html`, Cesium, milsymbol, etc. continue to flow into both paths.

---

## ISAK deployment (current)

Per [ISAK_Developer_Reference.md](ISAK_Developer_Reference.md): K3s + Traefik +
cert-manager, Iron Bank images only **in production**, non-root pods.

For day-to-day development against your own ISAK box you can sideload images
directly (no Iron Bank, no Harbor); see *Dev workflow* below. Iron Bank only
becomes mandatory at accreditation / fielded-install time.

### Dev workflow — build here, sideload to your ISAK

Pre-reqs: `docker`, `ssh`/`scp` access to the ISAK node, `helm` either locally
or on the node.

```bash
# One-shot: build, scp, k3s ctr images import
scripts/sideload.sh isak2.army.mil root dev

# Same, but also helm upgrade --install in isak-davi
DO_INSTALL=1 \
  ES_HOST=elasticsearch.isak-data.svc.cluster.local \
  TILES_HOST=tiles.isak-data.svc.cluster.local \
  scripts/sideload.sh isak2.army.mil root dev
```

The script uses the default (public) `nginx` base in the [Dockerfile](Dockerfile),
saves to a tarball, `scp`s it to the node, and runs
`k3s ctr images import` so containerd can see `davi:dev`. The chart is then
installed with `image.pullPolicy=Never` so the pod never tries to reach a
registry.

### Release workflow — Iron Bank build

When you're ready to submit for accreditation, rebuild against the Iron Bank
base via build args:

```bash
docker build \
  --build-arg BASE_IMAGE=registry1.dso.mil/ironbank/opensource/nginx/nginx \
  --build-arg BASE_TAG=1.27.3 \
  -t davi:0.1.0 .
docker tag davi:0.1.0 registry1.dso.mil/ironbank/davi/davi:0.1.0
```

### Install with Helm (release / Iron Bank path)

```bash
helm install davi ./charts/davi \
  --namespace isak-davi --create-namespace \
  --set hostname=isak2 \
  --set domain=army.mil \
  --set image.repository=registry1.dso.mil/ironbank/davi/davi \
  --set image.tag=0.1.0 \
  --set backends.elasticsearch.host=<es-svc>.<ns>.svc.cluster.local \
  --set backends.tiles.host=<tiles-svc>.<ns>.svc.cluster.local
```

App URL: `https://davi.public.<hostname>.<domain>`

### Flat manifest (no Helm)

For environments without Helm, [k8s/davi.yaml](k8s/davi.yaml) contains the
equivalent resources with `<HOSTNAME>`, `<DOMAIN>`, `<ES_HOST>`, `<TILES_HOST>`
placeholders to substitute before `kubectl apply -f`.

### Chart layout

```
charts/davi/
├── Chart.yaml
├── values.yaml
└── templates/
    ├── _helpers.tpl
    ├── namespace.yaml
    ├── configmap-nginx.yaml   # /elasticsearch and /tiles reverse-proxy
    ├── deployment.yaml        # non-root, readOnlyRootFilesystem
    ├── service.yaml           # ClusterIP :8080
    ├── ingress.yaml           # Traefik, websecure, TLS
    ├── certificate.yaml       # cert-manager via isak-ca-issuer
    └── NOTES.txt
```

### Notes / TODO

- Keycloak/OIDC is **not** enabled by default. `values.yaml` has a
  `keycloak:` block reserved for a future ForwardAuth / oauth2-proxy
  integration against the `isak` realm.
- The chart assumes Elasticsearch and a vector-tile service already exist
  in-cluster. If a `backends.*.host` value is empty, the matching nginx
  `location` block is omitted and that feature degrades gracefully.
- Until Harbor (ISAK Phase 3) is online, the DaVi image must be pre-loaded
  on the node (`ctr images import`) or pulled directly from Iron Bank.

---

## Legacy RDA path

Everything under [legacy/](legacy/) is the original JCRSD-style packaging
(rda bootstrap manifest, `DaVi.zip`/`DaViWorking.zip`, sample data files,
docs, the previous `Dockerfile` and `k8s/`). It is retained intact so the
old delivery path can keep being developed in parallel. See
[legacy/docs/RDA_DEPLOYMENT_OVERVIEW.md](legacy/docs/RDA_DEPLOYMENT_OVERVIEW.md)
and [legacy/docs/DaVi_PACKAGE_MANIFEST_REPORT.md](legacy/docs/DaVi_PACKAGE_MANIFEST_REPORT.md).