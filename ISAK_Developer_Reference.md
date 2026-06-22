# ISAK Developer Reference

**Platform:** TACTICAL Baseline v0.3.0  
**Kubernetes:** K3s v1.33.6 (single-node)  
**Build:** 20251229192322  
**Classification:** UNCLASSIFIED // FOR OFFICIAL USE

---

## Overview

ISAK (Integrated Services and Application Kubernetes) is a self-contained, air-gapped Kubernetes platform built on K3s. It is designed for tactical deployment on Army infrastructure. Every ISAK instance is a single-node K3s cluster with a fully automated install. Infrastructure components (IP addresses, FQDNs, hostnames) are defined at install time and vary per deployment — **no values should be hardcoded in application manifests.**

### Platform Stack

| Component | Technology | Version |
|---|---|---|
| Kubernetes | K3s | v1.33.6 |
| Ingress Controller | Traefik | v3.5.1 |
| Load Balancer | MetalLB (BGP) | v0.15.2 |
| Auth / SSO | Keycloak | 26.3.2 |
| Auth Database | PostgreSQL (Bitnami) | 17.4.0 |
| Storage Backend | Rook-Ceph | v1.18.8 / Ceph v19.2.3 |
| TLS Automation | cert-manager | v1.18.2 |
| Trust Distribution | trust-manager | v0.19.0 |
| Image Registry | Harbor | TBD — Phase 3 |
| OS Baseline | RHEL/Rocky (Kickstart) | STIG-hardened |
| Image Source | DoD Iron Bank | registry1.dso.mil |

---

## Environment Variables

The following values are installer-defined and differ per ISAK deployment. Never hardcode these. Reference them as variables or Helm values in all manifests.

| Variable | Description | Example Pattern |
|---|---|---|
| `INGRESS_FQDN` | Base domain for all app ingress | `public.isak2.army.mil` |
| `INGRESS_IP` | MetalLB external IP for all ingress | Assigned at install |
| `HOSTNAME` | Node hostname | `isak2` |
| `DOMAIN` | Network domain | `army.mil` |
| `KEYCLOAK_URL` | Full Keycloak base URL | `https://keycloak.public.<fqdn>` |

These values are stored on the node at:
- `/root/tacticalsetup/config/isak_inputs.json` — full install config
- `/root/tacticalsetup/config/ingress_info.txt` — ingress IP and FQDN

---

## Namespaces

### Convention

All ISAK application namespaces follow the pattern:

```
isak-<appname>
```

### Reserved Namespaces

| Namespace | Purpose |
|---|---|
| `isak-ceph` | Rook-Ceph storage stack |
| `isak-cert-manager` | cert-manager and trust-manager |
| `isak-http` | Node bootstrap file server |
| `isak-keycloak` | Keycloak SSO and PostgreSQL |
| `isak-metallb` | MetalLB load balancer |
| `kube-system` | K3s system components (Traefik, CoreDNS, metrics-server) |

Do not deploy application workloads into any reserved namespace.

---

## Ingress

### Pattern

All applications are exposed via Traefik using subdomains of the installer-defined FQDN:

```
https://<appname>.public.<hostname>.<domain>
```

Example: An app named `myapp` on `isak2.army.mil` would be:
```
https://myapp.public.isak2.army.mil
```

### Required Ingress Annotations

Every application Ingress resource **must** include these annotations:

```yaml
annotations:
  traefik.ingress.kubernetes.io/router.entrypoints: "websecure"
  traefik.ingress.kubernetes.io/router.tls: "true"
```

### Ingress Class

```yaml
ingressClassName: traefik
```

### TLS Certificate

Certificates are issued automatically by cert-manager. Reference the `isak-ca-issuer` ClusterIssuer in your manifest:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: myapp-tls
  namespace: isak-myapp
spec:
  secretName: myapp-tls-external
  issuerRef:
    name: isak-ca-issuer
    kind: ClusterIssuer
  commonName: "myapp.public.{{ .Values.hostname }}.{{ .Values.domain }}"
  dnsNames:
    - "myapp.public.{{ .Values.hostname }}.{{ .Values.domain }}"
  privateKey:
    algorithm: RSA
    size: 4096
    rotationPolicy: Always
  duration: 2160h
  renewBefore: 168h
```

### Full Ingress Example

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: myapp-ingress
  namespace: isak-myapp
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: "websecure"
    traefik.ingress.kubernetes.io/router.tls: "true"
spec:
  ingressClassName: traefik
  tls:
    - hosts:
        - "myapp.public.{{ .Values.hostname }}.{{ .Values.domain }}"
      secretName: myapp-tls-external
  rules:
    - host: "myapp.public.{{ .Values.hostname }}.{{ .Values.domain }}"
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: myapp-service
                port:
                  number: 8080
```

---

## Authentication — Keycloak / OIDC

### Realm

All applications authenticate against the `isak` realm.

| Setting | Value |
|---|---|
| Realm Name | `isak` |
| Keycloak Version | 26.3.2 |
| Protocol | OpenID Connect (OIDC) |
| OIDC Discovery URL | `https://keycloak.public.<fqdn>/realms/isak/.well-known/openid-configuration` |
| Authorization Endpoint | `https://keycloak.public.<fqdn>/realms/isak/protocol/openid-connect/auth` |
| Token Endpoint | `https://keycloak.public.<fqdn>/realms/isak/protocol/openid-connect/token` |
| Userinfo Endpoint | `https://keycloak.public.<fqdn>/realms/isak/protocol/openid-connect/userinfo` |
| Admin Console | `https://keycloak.public.<fqdn>/admin/isak/console/` |

### Token Configuration

| Setting | Value |
|---|---|
| Access Token Lifespan | 5 minutes |
| SSO Session Idle Timeout | 30 minutes |
| SSO Session Max Lifespan | 10 hours |
| Default Signature Algorithm | RS256 |

### Registering an Application Client

Each application must register a client in the `isak` realm. Self-registration is disabled — client registration must be performed manually via the Keycloak admin console or via the admin API.

**Recommended client settings:**

```json
{
  "clientId": "myapp",
  "enabled": true,
  "protocol": "openid-connect",
  "publicClient": false,
  "standardFlowEnabled": true,
  "implicitFlowEnabled": false,
  "directAccessGrantsEnabled": false,
  "serviceAccountsEnabled": false,
  "redirectUris": [
    "https://myapp.public.<fqdn>/*"
  ],
  "webOrigins": [
    "https://myapp.public.<fqdn>"
  ],
  "attributes": {
    "pkce.code.challenge.method": "S256"
  }
}
```

Store the client secret in a Kubernetes Secret in your app namespace and reference it via environment variable.

### Token Claims

The following claims are available in access tokens by default:

- `preferred_username` — username
- `email` — user email
- `given_name`, `family_name`, `full name`
- `realm_access.roles` — realm-level roles
- `resource_access.<client_id>.roles` — client-level roles
- `groups` — realm roles (via microprofile-jwt scope)

---

## Storage

Three Ceph-backed storage classes are available. Choose based on access pattern.

### Storage Classes

| Class | Provisioner | Access Mode | Use Case |
|---|---|---|---|
| `ceph-block` *(default)* | RBD (block) | RWO | Databases, stateful apps — one pod, one volume |
| `ceph-filesystem` | CephFS | RWX | Shared file access across multiple pods |
| `ceph-bucket` | RGW (object) | S3 API | Object/blob storage, file uploads |
| `local-path` | Node-local | RWO | Platform internals only — avoid for apps |

### PVC Example — Block Storage

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: myapp-data
  namespace: isak-myapp
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: ceph-block
  resources:
    requests:
      storage: 10Gi
```

### PVC Example — Shared Filesystem

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: myapp-shared
  namespace: isak-myapp
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ceph-filesystem
  resources:
    requests:
      storage: 10Gi
```

### Object Storage (S3-Compatible)

The Ceph RGW object store is available internally at:
```
http://rook-ceph-rgw-ceph-objectstore.isak-ceph.svc:80
```

S3 region is configured as `us-east-1`. Create an `ObjectBucketClaim` to provision a bucket:

```yaml
apiVersion: objectbucket.io/v1alpha1
kind: ObjectBucketClaim
metadata:
  name: myapp-bucket
  namespace: isak-myapp
spec:
  generateBucketName: myapp
  storageClassName: ceph-bucket
```

### Storage Limitations

> **Warning:** This is a single-node deployment with one OSD. All storage classes are configured with replication size 1 — no data redundancy. Data loss will occur if the OSD fails. This is expected for tactical/single-node deployments and is a known HEALTH_WARN in the Ceph cluster status.

---

## Container Requirements

### Image Source — Iron Bank Only

All container images **must** be sourced from DoD Iron Bank:

```
registry1.dso.mil/ironbank/<vendor>/<image>:<tag>
```

> **Harbor (Phase 3):** When deployed, Harbor will act as a proxy cache for Iron Bank, allowing nodes to pull images from Harbor instead of directly from `registry1.dso.mil`. The Harbor endpoint URL will be available post-deployment. Until Harbor is deployed, images must be pre-loaded or pulled directly from Iron Bank.

### Security Context — Required

All containers must run as non-root. The platform enforces this via STIG hardening and SELinux.

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 2000
  runAsGroup: 2000

podSecurityContext:
  fsGroup: 2000
```

### SELinux

SELinux is active on all nodes with MCS labels. Containers must be compatible with SELinux in enforcing mode. If a container requires elevated SELinux labels, document the requirement explicitly.

```yaml
securityContext:
  seLinuxOptions:
    level: "s0:c123,c456"
```

### Resource Requests and Limits

No cluster-wide ResourceQuota or LimitRange is enforced. However, all deployments should define requests and limits:

```yaml
resources:
  requests:
    cpu: "250m"
    memory: "256Mi"
  limits:
    cpu: "1000m"
    memory: "512Mi"
```

---

## Deploying Applications — Helm

### Deployment Method

Applications are deployed via Helm. There is no app store or automated pipeline at this time — deployments are applied directly to the cluster.

### Recommended Helm Values Structure

Parameterize all environment-specific values. A minimal `values.yaml` should include:

```yaml
# Environment-specific — set per deployment
hostname: "isak2"
domain: "army.mil"
ingressFqdn: "public.isak2.army.mil"

# Application config
image:
  repository: registry1.dso.mil/ironbank/<vendor>/<image>
  tag: "1.0.0"

replicaCount: 1

resources:
  requests:
    cpu: "250m"
    memory: "256Mi"
  limits:
    cpu: "1000m"
    memory: "512Mi"

keycloak:
  url: "https://keycloak.public.isak2.army.mil"
  realm: "isak"
  clientId: "myapp"

storage:
  size: "10Gi"
  class: "ceph-block"
```

### Recommended Manifest Layout

```
myapp/
├── Chart.yaml
├── values.yaml
├── templates/
│   ├── namespace.yaml
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── ingress.yaml
│   ├── certificate.yaml
│   ├── pvc.yaml
│   └── secret.yaml
```

### Install Command

```bash
helm install myapp ./myapp \
  --namespace isak-myapp \
  --create-namespace \
  --values values.yaml \
  --set hostname=isak2 \
  --set domain=army.mil
```

---

## Networking

### Internal Service DNS

Services are reachable across namespaces using standard Kubernetes DNS:

```
<service-name>.<namespace>.svc.cluster.local
```

Example — reaching Keycloak from another namespace:
```
keycloak-http.isak-keycloak.svc.cluster.local
```

### NetworkPolicy

No NetworkPolicies are enforced. Pods can communicate freely across all namespaces within the cluster.

### External Access

All external traffic enters through Traefik via the MetalLB-assigned load balancer IP. Applications are not exposed via NodePort or direct host networking — use Ingress resources only.

---

## Platform Services

### Ceph Dashboard

| Field | Value |
|---|---|
| URL | `https://ceph.public.<fqdn>` |
| Username | `admin` |
| Password | Stored in secret `rook-ceph-dashboard-password` in `isak-ceph` namespace |

Retrieve password:
```bash
kubectl get secret rook-ceph-dashboard-password -n isak-ceph \
  -o jsonpath="{['data']['password']}" | base64 --decode
```

### Keycloak Admin Console

| Field | Value |
|---|---|
| URL | `https://keycloak.public.<fqdn>/admin/isak/console/` |
| Username | `admin` |
| Default Password | `admin` — **change immediately after deployment** |

### Node Bootstrap Server

| Field | Value |
|---|---|
| URL | `https://http.public.<fqdn>` |
| Purpose | Serves `agent-token` and `k3s.yaml` for worker node onboarding |
| Auth | None — network-controlled access only |

---

## Known Limitations

| Limitation | Detail |
|---|---|
| Single node | Control plane and worker are the same node. No HA. |
| Single OSD | Ceph has no data redundancy. Replication size = 1 on all pools. |
| No RBAC model | No custom ClusterRoles defined yet. Role-based access model TBD. |
| No Harbor yet | Image registry is Phase 3. Iron Bank direct pull or pre-loaded images required until then. |
| Keycloak ingress disabled | Keycloak ingress is defined in Helm values but `enabled: false`. Access via internal DNS or manual ingress creation. |
| Air-gapped | No external internet access. All images, charts, and dependencies must be available locally. |
| Legacy Helm chart label | Keycloak Helm chart is labeled `17.0.1-legacy` but actual running version is 26.3.2. Use 26.x documentation. |

---

## Quick Reference

```bash
# Check cluster status
kubectl get nodes
kubectl get pods -A

# Check Ceph health
kubectl get cephcluster -n isak-ceph -o jsonpath='{.items[0].status.ceph.health}'

# Check ingress
kubectl get ingress -A

# Check storage
kubectl get pvc -A
kubectl get storageclass

# Check Helm releases
helm list -A

# Get Ceph dashboard password
kubectl get secret rook-ceph-dashboard-password -n isak-ceph \
  -o jsonpath="{['data']['password']}" | base64 --decode

# Get ingress IP and FQDN
cat /root/tacticalsetup/config/ingress_info.txt

# Get full install config
cat /root/tacticalsetup/config/isak_inputs.json
```

---

*Last updated: 22 MAY 2026 — Based on TACTICAL Baseline v0.3.0, Build 20251229192322*  
*Captured from: isak2.army.mil*
