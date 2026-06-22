# DaVi on TDP — Deployment & Internal-Use Instructions

**Artifact:** [DaVi.zip](../DaVi.zip) (legacy RDA package)
**Target platform:** TDP (Tactical Data Platform) — pre-JCRSD environment
**App key:** `davi.viewer`
**Context path:** `/davi`
**Status:** Legacy reference — the live target is JCRSD. See [RDA_DEPLOYMENT_OVERVIEW.md](RDA_DEPLOYMENT_OVERVIEW.md) for the current environment.

> Use this document when re-deploying the old DaVi.zip onto a TDP-style RDA host, or when troubleshooting behavior that matches the original TDP build (HTTP origin, Keycloak Bearer tokens, `/datafabric/...` ES proxy).

---

## 1) What the artifact is

DaVi.zip is a static-content RDA package. Flat structure:

```
DaVi.zip
├── initial.config       # navigatorApp registration for the TDP app launcher
├── rda.manifest         # appKey, context, file list, proxy type
└── content/
    ├── index.html       # the entire DaVi UI (single-file app)
    ├── manifest.json    # PWA manifest
    ├── DaVi_Icon.png
    ├── davi_icon.png
    ├── Cesium/          # CesiumJS 1.140 + Assets/Textures/NaturalEarthII (bundled)
    ├── milsymbol/       # milsymbol.js (MIL-STD-2525 renderer)
    └── mvt/             # mvt.bundle.js (pako + Pbf + VectorTile)
```

Key facts:
- The full DaVi UI lives in [content/index.html](../davi-v2/src/index.html). It is a single-file app — no build step.
- Cesium, Natural Earth tiles, milsymbol, and the MVT parser are bundled. The package is self-contained for offline/air-gapped TDP hosts.
- Source-of-truth for the in-zip files is [davi-v2/](../davi-v2/). See [DaVi_PACKAGE_MANIFEST_REPORT.md](DaVi_PACKAGE_MANIFEST_REPORT.md) for SHA-256s.

---

## 2) Pre-flight checks

Confirm before deploying:

| Item | Expected on TDP | How to verify |
|---|---|---|
| RDA Deployer reachable | `http(s)://<tdp-host>/deployer/` | Browser → deployer UI loads |
| App slot `/davi` is free | No prior `davi.viewer` registration | RDA Deployer → installed apps list |
| Backend `/elasticsearch/` route | Returns 200/401 (not 404) | `curl -k http(s)://<tdp-host>/elasticsearch/` |
| Backend `/datafabric/api/elastic/v1/` route | Returns 200/401/403 (not 404) | `curl -k http(s)://<tdp-host>/datafabric/api/elastic/v1/` |
| Keycloak realm reachable | Token endpoint returns JWT | `curl` against the realm's `/protocol/openid-connect/token` |
| `/Build/CesiumUnminified/` on host nginx | Only required if using **host-served** Cesium (older flavor) | `ls /var/www/html/Build/CesiumUnminified/` |

> The shipped DaVi.zip bundles its own Cesium under `content/Cesium/`. The host-side `/Build/CesiumUnminified/` path is only relevant if you are deploying a pre-bundle variant.

---

## 3) Deployment procedure (RDA Deployer UI)

1. **Sign in** to the RDA Deployer at `http(s)://<tdp-host>/deployer/`.
2. **Upload** [DaVi.zip](../DaVi.zip) via the deployer's *Install / Upload Package* action.
3. The deployer reads `rda.manifest` and registers the `static-content` component:
   - `appKey`: `davi.viewer`
   - `context`: `/davi`
   - `proxy.type`: `primary`
4. The deployer reads `initial.config` and adds the entry to the TDP app launcher (Navigator) under the `data` category, with the icon `davi_icon.png`.
5. Wait for the deployer to report **Healthy**. The pod that backs the static content will be named on the pattern `davi-viewer-<random>-davi-*` and runs nginx serving the unpacked content from the RDA resource volume.
6. Open the app at: `http(s)://<tdp-host>/davi/index.html` (or via the Navigator tile).

### Re-deploy / upgrade

1. In the RDA Deployer, **Uninstall** the existing `davi.viewer` registration.
2. Upload the new DaVi.zip.
3. If the deployer supports in-place upgrade, that is preferred; uninstall/reinstall is the safe fallback.

### Rollback

Two prior known-good builds are kept alongside the current zip for quick rollback:
- [DaVi.zip.prev](../DaVi.zip.prev)
- [DaVi.zip.bak](../DaVi.zip.bak)

Both are v2.1.0 of the same package structure. Rename to `DaVi.zip` before re-uploading.

---

## 4) TDP host paths (reference)

Once installed, these are the runtime paths observed on the TDP host during the original deployment. Useful for SCP/WinSCP recovery if the deployer is unavailable.

| Path | Purpose |
|---|---|
| `/var/www/html/Apps/` | Host-side static-app drop (older flavor; `HelloWorld.html` was DaVi's first name here) |
| `/etc/nginx/html/` | Runtime web root the live pod was actually serving from |
| `/var/www/html/Build/CesiumUnminified/` | Host-supplied Cesium 1.140 (legacy; superseded by bundled Cesium in v2.1.0) |
| `/var/www/html/Build/CesiumUnminified/Assets/Textures/NaturalEarthII/` | Host-supplied Natural Earth tiles (legacy) |
| `/mnt/ltac/nginx/locations/location-*.conf` | Per-app nginx `location` snippets (e.g. `location-davi.conf`, `location-tdp-data-catalog.conf`) |
| `/rda-resources/` (inside the pod) | The unzipped `content/` directory the deployer mounts |

To extract a copy of the running app from a TDP pod:

```bash
kubectl exec -n <ns> <davi-pod> -- tar -czf - /rda-resources | tar -xzvf -
```

---

## 5) Backend wiring (what DaVi expects on TDP)

DaVi calls everything via **relative URLs**. The nginx layer in front of the RDA must proxy:

| Browser path | Forwarded to | Used by |
|---|---|---|
| `/davi/...` | This RDA's static content | UI assets, index.html, Cesium, milsymbol, mvt |
| `/elasticsearch/...` | Elasticsearch (cluster-internal) | Index Scanner, "Browse Indexes", scan/plot direct calls |
| `/datafabric/api/elastic/v1/...` | Datafabric ES proxy (TDP-specific) | "Plot from Index" export + Live Feed polling |
| Custom `X-BDP-Redirect: false` header | (passed through) | Suppresses TDP datafabric session redirects on ES queries |

On TDP the typical `location-davi.conf` snippet dropped at `/mnt/ltac/nginx/locations/` proxies `/elasticsearch/` and `/datafabric/` to the upstream services with the realm's bearer token forwarded. If a fresh TDP host is missing the `/datafabric/` block, the **Plot from Index** and **Live Feed** features will return 404 — see Troubleshooting.

---

## 6) Authentication (Keycloak / Bearer token)

The original TDP build uses Keycloak directly from the browser:

1. User authenticates against Keycloak (redirect or pre-existing SSO session).
2. The DaVi page fetches the JWT and stores it client-side.
3. Each `/elasticsearch/...` and `/datafabric/...` request is sent with:
   - `Authorization: Bearer <access_token>`
   - `X-BDP-Redirect: false`
4. A countdown timer + auto-refresh toggle keeps the token alive across long sessions.

> The Keycloak token-fetch path is the obvious TDP marker in the code. The JCRSD rebuild rips this out and rides the Citadel session cookie via `credentials: "include"`. If you ever cross-deploy a JCRSD-flavored zip onto TDP, ES calls will hit 401 because no Bearer is attached.

---

## 7) Internal-use guide (what operators see in the UI)

Open `/davi/index.html`. The left panel exposes these sections, in order:

### Map Source
- **Default:** Natural Earth II (local, bundled). Works fully offline — required on TDP because the host is HTTP-only and Cesium Ion is blocked by browser mixed-content rules.
- **Cesium Ion (Aerial / Aerial w/ Labels / Road):** Requires the host to be HTTPS *and* outbound egress to `api.cesium.com` and `assets.ion.cesium.com`. **On TDP this will silently fail** — leave it on Natural Earth or use a Custom TMS.
- **Custom TMS / WMTS URL Template:** Paste an internal tile-server URL (e.g. an MVT or raster tile service hosted elsewhere on the enclave). Press **Apply Map Source**.
- The `TDP // DataViewer` strap in the panel header is the visual confirmation you are on the TDP-flavored build.

### Index Scanner
- Click **Scan All Indexes** to query Elasticsearch via `/elasticsearch/_cat/indices` and `/elasticsearch/<idx>/_search`. Indexes that contain detectable lat/lon are surfaced with data-type chips.
- **KEYWORD FILTER** narrows the result list by index name or field keyword.
- **SCAN ALL DOCUMENTS** removes the sampling cap (slower; use only when you need exhaustive coverage).
- **AUTO-RESCAN** repeats the scan on a fixed interval (1 / 5 / 10 / 30 min). The pulsing green dot confirms the timer is armed.

### Index Browser
- **Browse Indexes** loads the raw `_cat/indices` list for manual selection. Use this when the scanner's data-type detection is wrong and you know exactly which index you want.

### Manual Override (Field Mapping)
- Expand to set non-default field names: **Latitude Field** (default `latitude`), **Longitude Field** (default `longitude`), **Label Field** (default `name`).
- **Plot on Globe** runs the query and renders points. **Clear Points** removes them.

### Local File Upload
- Drop a `.slf`, `.kml`, `.kmz`, `.csv`, `.xml`, `.json`, or `.ndjson` file directly onto the map — no Elasticsearch required.
- Useful on TDP for previewing AFATDS / LOGSTAT / NTC GCM exports without scanning ES. Sample files are in [legacy/](..).

### Live Feed
- Poll interval: 10 s / 30 s / 60 s / 5 min.
- Toggle ON to begin polling `/datafabric/api/elastic/v1/...` for new docs since the last poll's timestamp. The detected **Timestamp Field** is shown read-only.
- On TDP this hits the datafabric proxy. If `/datafabric/` is not configured on the host's nginx, the feature is a no-op (silent 404s in the network tab).

---

## 8) Troubleshooting (TDP-specific)

| Symptom | Cause | Fix |
|---|---|---|
| Globe loads but the world is blue/empty | Default Ion basemap selected on an HTTP TDP host → mixed-content block | Switch **Map Source** to **Natural Earth II** and press Apply |
| All `/elasticsearch/` calls return 401 | Keycloak token not attached or expired | Re-auth via the platform login; verify the countdown timer is visible and counting |
| **Plot from Index** or **Live Feed** silently does nothing | TDP host missing `/datafabric/api/elastic/v1/` `location` block | Add `location-davi.conf` at `/mnt/ltac/nginx/locations/` proxying `/datafabric/` to the datafabric service |
| Navigator launcher tile missing | `initial.config` not registered | Reinstall the RDA; deployer must consume `initial.config` to surface the tile |
| Cesium worker 404s in DevTools | RDA upload truncated; `content/Cesium/Workers/` incomplete | Re-upload the full DaVi.zip (verify SHA-256 against [DaVi_PACKAGE_MANIFEST_REPORT.md](DaVi_PACKAGE_MANIFEST_REPORT.md)) |
| Pod shows old assets after redeploy | nginx still serving from `/etc/nginx/html` with cached files | Scale the `davi-viewer-*-davi` deployment to 0 then back to 1; the init container restages `/rda-resources/` |

---

## 9) Verifying a successful TDP deployment

After install, the following should all be true:

1. `http(s)://<tdp-host>/davi/index.html` loads and shows the **TDP // DataViewer** header.
2. DevTools → Network: `GET /davi/Cesium/Cesium.js` returns 200.
3. DevTools → Network: `GET /davi/mvt/mvt.bundle.js` returns 200.
4. DevTools → Network: `GET /davi/milsymbol/milsymbol.js` returns 200.
5. **Scan All Indexes** returns at least one index from ES with an `Authorization: Bearer ...` header attached.
6. The TDP Navigator shows the DaVi tile under the **data** category.

If all six pass, the TDP deployment is healthy.

---

## 10) See also

- [RDA_DEPLOYMENT_OVERVIEW.md](RDA_DEPLOYMENT_OVERVIEW.md) — current JCRSD-targeted overview
- [DaVi_PACKAGE_MANIFEST_REPORT.md](DaVi_PACKAGE_MANIFEST_REPORT.md) — file inventory + SHA-256s
- [davi-v2/src/index.html](../davi-v2/src/index.html) — the single-file UI source
- [davi-v2/rda/manifest.json](../davi-v2/rda/manifest.json) — manifest source-of-truth
- [davi-v2/rda/bootstrap.manifest](../davi-v2/rda/bootstrap.manifest) — bootstrap nginx config template
