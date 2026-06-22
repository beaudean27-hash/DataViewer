# DaVi RDA Deployment Overview (Plain Language)

This document explains how the DaVi RDA launches, how it starts a Kubernetes pod, and how it connects to Elasticsearch and map services.

The goal is to give developers a simple mental model of "what starts where" and "how the app knows where to connect."

## 1) What gets deployed

At runtime, DaVi is mainly a web app served by nginx.

What is inside the deployment:
- The DaVi web page and UI files
- Cesium and map rendering assets
- Supporting map/vector helper files
- nginx configuration used to route backend requests

In this repository, the container build starts from:
- [Dockerfile](../Dockerfile)

## 2) High-level launch flow

From a platform perspective, launch usually works like this:

1. The RDA package is installed in the environment.
2. Kubernetes starts the DaVi workload.
3. Init/bootstrap logic copies app files into the nginx web root.
4. nginx starts and serves the app.
5. The browser opens DaVi and makes same-origin requests for data.

Important point:
- The running pod observed in your environment had init containers and bootstrap behavior, which means the final web files are staged during startup (not only baked into the base nginx image).

## 3) Where DaVi serves files from at runtime

In your live environment, the app content was being served from:
- /etc/nginx/html

This explains why /usr/share/nginx/html looked small in the running pod.

## 4) How DaVi talks to Elasticsearch

DaVi calls Elasticsearch using a relative path, not a hardcoded host.

Examples from the app code:
- [davi-v2/src/index.html](../davi-v2/src/index.html#L431)
- [davi-v2/src/index.html](../davi-v2/src/index.html#L479)

Those calls look like:
- /elasticsearch/... 

Meaning:
- The browser sends requests to the same server that served the page.
- nginx (or platform gateway/proxy in front of it) decides where to forward /elasticsearch.

## 5) How DaVi talks to map tile services

DaVi also uses relative paths for map tiles in the default JCRSD source.

Example:
- [davi-v2/src/index.html](../davi-v2/src/index.html#L325)

That path is:
- /tiles/data/planet/{z}/{x}/{y}.pbf

Meaning:
- The app itself does not need a hardcoded tile server IP.
- Routing is handled by environment-specific nginx/proxy settings.

## 6) How it knows server IPs (or hostnames)

Short answer:
- The app does not directly store fixed Elasticsearch or tile server IPs in code.

Instead, environment routing comes from config:
- nginx config used by the RDA bootstrap
- platform/network configuration
- Kubernetes service and ingress/proxy setup

The bootstrap manifest in this repo shows that nginx config is templated and injected during startup:
- [davi-v2/rda/bootstrap.manifest](../davi-v2/rda/bootstrap.manifest)

In other words:
- Developers keep app calls generic (for example /elasticsearch/...)
- Admin/platform config maps those paths to real internal servers for that site

## 7) Which Kubernetes object controls pod on/off

In your environment, the running pod was controlled by this deployment:
- davi-viewer-mivopuolpgir-davi

So scaling that deployment to 0 turns the pod off, and scaling back to 1 turns it on.

## 8) What to tell developers (simple checklist)

When developers ask "how does DaVi launch and connect?":

1. DaVi is a web UI served by nginx inside Kubernetes.
2. Startup bootstrap copies app assets into nginx web root.
3. The app calls backend paths like /elasticsearch and /tiles.
4. The environment (nginx/proxy config) decides actual backend server targets.
5. No site-specific backend IP should be hardcoded in frontend logic.

## 9) If something breaks

Common troubleshooting direction:

1. Confirm the deployment is running in the correct namespace.
2. Confirm nginx is serving from the expected web root.
3. Confirm /elasticsearch and /tiles routes are configured correctly in environment config.
4. Confirm backend services are reachable from cluster network.

---

If needed, this document can be expanded with a second, technical appendix that includes exact commands and object names per environment.
