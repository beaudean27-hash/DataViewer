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

### Data backends

DaVi reads two kinds of dataset:

| Backend | Browser path | Server | Notes |
|---|---|---|---|
| Elasticsearch indexes | `/elasticsearch/*` | nginx → ES cluster IP | Original backend; `_cat/indices`, `_search`, `_mapping`. |
| PostgreSQL tables (new) | `/postgres/*` | nginx → PostgREST → PG | OpenAPI discovery, `GET /{table}` for rows, `Accept: application/geo+json` for PostGIS tables, ilike-based keyword search. |
| Other ISAK Tools (new) | `/extras/<name>/*` + `/discover/static.json` | nginx → in-cluster Services | Static catalog of neighboring ISAK apps (OGC/WFS, TAK Marti, MinIO, generic REST, secondary ES, Kibana link-out). Configure via `backends.extras[]`. |

Both surface in the side panel as parallel selection lists ("Index Browser"
and "PostgreSQL Tables"). Selected datasets from either backend feed the same
renderers, KW doc browser, and Cesium entities — no separate code paths for
SIDC decoding, shape geometry, or labels. The Postgres adapter normalizes
rows into the DaVi record shape (`{data, id}`), promoting any GeoJSON
`geometry` Point to top-level `latitude`/`longitude` and lifting columns named
`geom` / `geometry` / `shape` for the existing `extractShapeGeometry` /
`extractSymbolMeta` pipeline.

A third side-panel list — **"Other ISAK Tools"** — is driven by
`/discover/static.json`, a catalog the chart generates from `backends.extras[]`.
The browser probes each registered tool with a type-specific adapter
(OGC GetCapabilities, S3 ListObjectsV2, PostgREST-style OpenAPI for secondary
ES, TAK `Marti/api/missions`, generic REST root walk) and lists per-tool
datasets the operator can select and plot. Selected datasets are pre-fetched
into `_exCachedRecords` under an `ex::<tool>/<dataset>` key; `plotPoints`,
`openKwDocBrowser`, `pollForNewData`, and `detectTimestampField` short-circuit
on `exIsKey()` exactly like the PG path, so every record flows through the
same renderer / popup / KW pipeline. Link-out-only entries (e.g. Kibana) skip
the nginx proxy and open in a new tab from the side panel.

Live-feed polling spans all three backends. PG datasets with a known
timestamp column use `?<col>=gt.<lastTs>&order=<col>.desc&limit=500`; EX
datasets re-invoke their per-type adapter on a per-type cooldown
(elasticsearch/rest poll every cycle, TAK every 5 s, OGC every 30 s,
MinIO every 60 s; Kibana never polls). Dedupe is unified through the
existing `seenLiveDocIds` table.

The keyword document browser will additionally try server-side typed
search (`?q=`, then `?search=`, then `?filter=`) for EX entries of type
`rest`, and only falls back to the client-side substring scan when the
server appears to ignore the parameter (returns the same row count as the
unfiltered call).

To register tools, add entries under `backends.extras` in `values.yaml`
(or via `--set-file`). Example:

```yaml
backends:
  extras:
    - name: geoserver
      label: "GeoServer (OGC)"
      type: ogc
      service: "geoserver.isak-gis.svc.cluster.local"
      port: 8080
      basePath: "/geoserver"
      hints: { wfsPath: "wfs", wmsPath: "wms" }
    - name: takserver
      label: "TAK Server"
      type: tak
      service: "takserver.isak-tak.svc.cluster.local"
      port: 8443
      scheme: "https"
      hints: { apiBase: "Marti/api" }
    - name: kibana
      label: "Kibana"
      type: kibana
      linkOut: "https://kibana.public.isak2.army.mil"
```

Supported `type` values: `ogc`, `tak`, `minio`, `kibana`, `rest`,
`elasticsearch`, `unknown`. The `unknown` type is reserved for entries
emitted by the active discovery sidecar (Option B) when the prober can't
narrow the backend to one of the known kinds.

### Active discovery sidecar (Option B)

In addition to the operator-curated `backends.extras[]` list, DaVi can run a
small in-pod sidecar that watches the K8s API for neighbouring Services in
configured namespaces, probes them for known data-API signatures, and merges
the discovered set into the same `/discover/static.json` the browser already
consumes.

The sidecar is Go stdlib only, runs from a distroless image as nonroot
UID 65532 with `readOnlyRootFilesystem` + `RuntimeDefault` seccomp, and asks
the K8s API for `services`, `endpoints`, and `namespaces` (get/list) — no
pods, no secrets. It exposes `GET /static.json`, `/healthz` (always 200), and
`/readyz` (503 "warming" until the first refresh completes). The chart
points nginx's `location = /discover/static.json` at `127.0.0.1:9090`
whenever the sidecar is enabled; the static ConfigMap is mounted into the
sidecar as the seed catalog so static entries always survive K8s API
hiccups.

Merge semantics: a discovered entry is suppressed (`shadowsStatic()`) when
any static entry references the same `service` FQDN + `port`. The output
is sorted static-first then alphabetically.

Enable via Helm:

```yaml
discovery:
  sidecar:
    enabled: true
    namespaceIncludes: ["isak-*"]
    namespaceExcludes: ["kube-system", "kube-public"]
    probeEnabled: true
    refreshSeconds: 60
    probeTimeoutMs: 3000
```

…or via the sideload script (which builds, ships, and helm-installs in one shot):

```bash
WITH_SIDECAR=1 DO_INSTALL=1 \
  scripts/sideload.sh isak2.army.mil root dev
```

When the sidecar fails to read its in-cluster ServiceAccount token (e.g.
RBAC not yet applied), it logs a warning and continues to serve the static
catalog with `source:"discover-sidecar (no-kube)"` instead of crashing.

---

## ISAK deployment (current)

Per [ISAK_Developer_Reference.md](ISAK_Developer_Reference.md): K3s + Traefik +
cert-manager, Iron Bank images only **in production**, non-root pods.

For day-to-day development against your own ISAK box you can sideload images
directly (no Iron Bank, no Harbor); see *Dev workflow* below. Iron Bank only
becomes mandatory at accreditation / fielded-install time.

For distributing a build to other ISAK installs (operator hands it off,
deploys at a different site) see *Shareable install package* below.

### Dev workflow — build here, sideload to your ISAK

Pre-reqs: `docker`, `ssh`/`scp` access to the ISAK node, `helm` either locally
or on the node.

```bash
# One-shot: build, scp, k3s ctr images import
scripts/sideload.sh isak2.army.mil root dev

# Same, but also helm upgrade --install in isak-davi (with discovery sidecar)
WITH_SIDECAR=1 DO_INSTALL=1 \
  scripts/sideload.sh isak2.army.mil root v0.3.0-sidecar
```

### Shareable install package — build once, deploy anywhere

Produces a single self-contained tarball that an operator can carry to any
ISAK install (USB stick, air-gap copy, etc.) and run with one command on the
node.

```bash
# On your workstation (needs docker)
scripts/build-isak-package.sh v0.3.0-sidecar
# → dist/davi-isak-v0.3.0-sidecar.tar.gz (~30 MB)
```

The tarball contains both image tarballs (main app + discovery sidecar), the
Helm chart, an operator README, and an `install.sh` that auto-detects
`HOSTNAME`/`DOMAIN` from `/root/tacticalsetup/config/isak_inputs.json`,
imports the images into K3s containerd, and runs `helm upgrade --install`.

```bash
# Operator, on the ISAK node:
scp davi-isak-v0.3.0-sidecar.tar.gz root@<isak-host>:/root/
ssh root@<isak-host>
cd /root && tar xzf davi-isak-v0.3.0-sidecar.tar.gz
cd davi-isak-v0.3.0-sidecar
sha256sum -c SHA256SUMS
./install.sh
```

Postgres is opt-in. To bring up an in-chart PostgREST gateway pointed at an
existing PG service:

```bash
helm upgrade --install davi ./charts/davi \
  --namespace isak-davi --create-namespace \
  --set hostname=isak2 --set domain=army.mil \
  --set backends.postgres.host=postgres.isak-data.svc.cluster.local \
  --set backends.postgres.db=davi \
  --set backends.postgres.schema=davi \
  --set postgrest.enabled=true \
  --set postgrest.connectionString="postgres://davi_ro:<pw>@postgres.isak-data.svc.cluster.local:5432/davi"
```

Or skip the in-chart PostgREST and point at an external one:

```bash
  --set backends.postgres.gatewayHost=postgrest.isak-data.svc.cluster.local
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
- PostgREST is the only supported HTTP shim for Postgres in the chart;
  set `postgrest.enabled=true` for the in-chart Deployment + Service, or
  point `backends.postgres.gatewayHost` at an external PostgREST. The
  client side recognizes both PostGIS geometry columns and plain
  lat/lon / GeoJSON-jsonb tables; tables outside `backends.postgres.schema`
  are invisible by design.
- PG live-feed / scan-promotion / mapping-driven keyword search are
  follow-up work; first pass uses sample-based detection and ilike on
  every text column.
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