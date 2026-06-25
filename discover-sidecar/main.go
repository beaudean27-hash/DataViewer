// davi-discover — DaVi Option-B discovery sidecar.
//
// Runs alongside the DaVi nginx container in the same pod. Lists Services in
// configured namespaces via the in-cluster Kubernetes API (using the pod's
// ServiceAccount token), classifies each by issuing read-only HTTP probes for
// known data-API signatures (Elasticsearch, PostgREST, OGC GeoServer, MinIO,
// TAK Server, generic REST), merges the result with the operator-curated
// static catalog mounted at /etc/davi-discover/static.json, and serves the
// combined catalog at GET /static.json so the browser's existing
// /discover/static.json fetch keeps working unchanged.
//
// Static entries always win on name collisions. Discovered entries carry a
// `"discovered": true` marker so the UI can badge them.
//
// Stdlib-only, no external deps. Distroless-static base, runs as non-root.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultPort           = "9090"
	defaultRefreshSec     = 60
	defaultProbeTimeoutMs = 3000
	saTokenPath           = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath              = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	kubeAPI               = "https://kubernetes.default.svc"
)

// ─── Catalog schema (mirrors /discover/static.json that the UI parses) ────

type CatalogEntry struct {
	Name       string                 `json:"name"`
	Label      string                 `json:"label"`
	Type       string                 `json:"type"`
	ProxyPath  string                 `json:"proxyPath,omitempty"`
	LinkOut    string                 `json:"linkOut,omitempty"`
	Hints      map[string]interface{} `json:"hints,omitempty"`
	Discovered bool                   `json:"discovered,omitempty"`
	Source     string                 `json:"source,omitempty"` // "static" | "discovered"
	// Internal: which Service this discovered entry maps to. Not surfaced.
	internalSvc *Service `json:"-"`
}

type Catalog struct {
	Version     int            `json:"version"`
	Source      string         `json:"source"`
	GeneratedAt string         `json:"generatedAt"`
	Entries     []CatalogEntry `json:"entries"`
}

// ─── K8s Services we care about ───────────────────────────────────────────

type Service struct {
	Namespace string
	Name      string
	FQDN      string
	Ports     []ServicePort
}

type ServicePort struct {
	Name     string
	Port     int
	Protocol string
}

// ─── Configuration ────────────────────────────────────────────────────────

type Config struct {
	Port            string
	RefreshInterval time.Duration
	ProbeTimeout    time.Duration
	NSIncludeGlob   []string // e.g. ["isak-*"]
	NSExclude       []string // exact names
	StaticPath      string
	ProbeEnabled    bool
	OwnNamespace    string // skip Services in this NS (avoid self-loop)

	// TAK mTLS + admin upload
	TAKMountPath  string // dir containing client.crt/key (PEM) or client.p12 (P12)
	TAKSecretName string // K8s Secret to PATCH on admin upload
	AdminToken    string // X-DaVi-Admin-Token gate for /admin/*; empty disables
}

func loadConfig() Config {
	cfg := Config{
		Port:            envDefault("DAVI_DISCOVER_PORT", defaultPort),
		RefreshInterval: time.Duration(envInt("DAVI_DISCOVER_REFRESH_SEC", defaultRefreshSec)) * time.Second,
		ProbeTimeout:    time.Duration(envInt("DAVI_DISCOVER_PROBE_TIMEOUT_MS", defaultProbeTimeoutMs)) * time.Millisecond,
		NSIncludeGlob:   splitCSV(envDefault("DAVI_DISCOVER_NS_INCLUDE", "isak-*")),
		NSExclude:       splitCSV(envDefault("DAVI_DISCOVER_NS_EXCLUDE", "isak-cert-manager,isak-keycloak,isak-metallb,isak-ceph,isak-http,kube-system,kube-public,kube-node-lease,default")),
		StaticPath:      envDefault("DAVI_DISCOVER_STATIC_PATH", "/etc/davi-discover/static.json"),
		ProbeEnabled:    envBool("DAVI_DISCOVER_PROBE_ENABLED", true),
		OwnNamespace:    envDefault("POD_NAMESPACE", ""),
		TAKMountPath:    envDefault("DAVI_TAK_MOUNT_PATH", defaultTAKMountPath),
		TAKSecretName:   envDefault("DAVI_TAK_SECRET_NAME", ""),
		AdminToken:      strings.TrimSpace(os.Getenv("DAVI_ADMIN_TOKEN")),
	}
	return cfg
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		v = strings.ToLower(v)
		return v == "1" || v == "true" || v == "yes" || v == "on"
	}
	return def
}
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ─── In-cluster K8s API client (stdlib-only) ──────────────────────────────

type kubeClient struct {
	httpC   *http.Client
	token   string
	baseURL string
}

func newKubeClient(timeout time.Duration) (*kubeClient, error) {
	caPEM, err := os.ReadFile(saCAPath)
	if err != nil {
		return nil, fmt.Errorf("read SA ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse SA ca failed")
	}
	tok, err := os.ReadFile(saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read SA token: %w", err)
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
		Proxy: nil,
	}
	return &kubeClient{
		httpC:   &http.Client{Transport: tr, Timeout: timeout},
		token:   strings.TrimSpace(string(tok)),
		baseURL: kubeAPI,
	}, nil
}

// k8sServicesResp is the slim shape of `GET /api/v1/services` we use.
type k8sServicesResp struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Type  string `json:"type"`
			Ports []struct {
				Name     string `json:"name"`
				Port     int    `json:"port"`
				Protocol string `json:"protocol"`
			} `json:"ports"`
		} `json:"spec"`
	} `json:"items"`
}

func (k *kubeClient) listServices(ctx context.Context) ([]Service, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", k.baseURL+"/api/v1/services?limit=1000", nil)
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	resp, err := k.httpC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("services list HTTP %d: %s", resp.StatusCode, string(body))
	}
	var sr k8sServicesResp
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	out := make([]Service, 0, len(sr.Items))
	for _, it := range sr.Items {
		svc := Service{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			FQDN:      fmt.Sprintf("%s.%s.svc.cluster.local", it.Metadata.Name, it.Metadata.Namespace),
		}
		for _, p := range it.Spec.Ports {
			svc.Ports = append(svc.Ports, ServicePort{Name: p.Name, Port: p.Port, Protocol: p.Protocol})
		}
		out = append(out, svc)
	}
	return out, nil
}

// patchSecretData updates the `data` map of an existing Secret. Values are
// raw bytes; this function base64-encodes them before sending. Existing keys
// not present in `data` are preserved (strategic merge patch on Secret data
// adds/updates keys but does not remove unmentioned ones).
//
// Returns an error when the Secret does not exist (operator must pre-create
// it via Helm); we do NOT auto-create here to keep RBAC scope tight (no
// secrets/create needed — only get/patch).
func (k *kubeClient) patchSecretData(ctx context.Context, namespace, name string, data map[string][]byte) error {
	if namespace == "" {
		return errors.New("kube: namespace is empty (POD_NAMESPACE not set?)")
	}
	if name == "" {
		return errors.New("kube: secret name is empty")
	}
	// Build strategic-merge patch body.
	enc := map[string]string{}
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString(v)
	}
	body, err := json.Marshal(map[string]interface{}{"data": enc})
	if err != nil {
		return err
	}
	urlStr := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", k.baseURL, namespace, name)
	req, _ := http.NewRequestWithContext(ctx, "PATCH", urlStr, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	req.Header.Set("Accept", "application/json")
	resp, err := k.httpC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("PATCH secret HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// ─── Namespace filtering ──────────────────────────────────────────────────

// nsMatches returns true when ns matches any include pattern AND none of the
// excludes AND is not the sidecar's own namespace. Includes support a single
// trailing `*` wildcard (e.g. "isak-*").
func nsMatches(ns string, includes, excludes []string, own string) bool {
	if ns == own && own != "" {
		return false
	}
	for _, e := range excludes {
		if e == ns {
			return false
		}
	}
	if len(includes) == 0 {
		return true
	}
	for _, inc := range includes {
		if inc == ns {
			return true
		}
		if strings.HasSuffix(inc, "*") {
			prefix := strings.TrimSuffix(inc, "*")
			if strings.HasPrefix(ns, prefix) {
				return true
			}
		}
	}
	return false
}

// ─── Probers: classify a Service by hitting known endpoints ───────────────

type proberFunc func(ctx context.Context, c *http.Client, svc Service, port ServicePort) (CatalogEntry, bool)

// probers are tried in order; first hit wins. Cheap/specific probes go first.
var probers = []proberFunc{
	probeElasticsearch,
	probePostgREST,
	probeMinIO,
	probeTAK,
	probeOGC,
	probeREST, // last resort: any JSON-returning root
}

func probeService(ctx context.Context, c *http.Client, svc Service) (CatalogEntry, bool) {
	// Try each (scheme, port) tuple. Most cluster services are HTTP on the
	// declared port; TAK is HTTPS; MinIO/PostgREST/ES are HTTP.
	for _, p := range svc.Ports {
		// Skip non-TCP and obviously non-HTTP ports.
		if p.Protocol != "" && p.Protocol != "TCP" {
			continue
		}
		// Don't probe DaVi's own nginx (port 8080 on davi service).
		if strings.Contains(strings.ToLower(svc.Name), "davi") && p.Port == 8080 {
			continue
		}
		for _, prober := range probers {
			ent, ok := prober(ctx, c, svc, p)
			if ok {
				ent.internalSvc = &svc
				return ent, true
			}
		}
	}
	return CatalogEntry{}, false
}

// httpGet performs a probe GET with a tight timeout. Errors are demoted to
// (nil, status=0) so a single network blip doesn't poison classification.
func httpGet(ctx context.Context, c *http.Client, scheme, host string, port int, path string, accept string) (status int, body []byte, ctype string) {
	urlStr := fmt.Sprintf("%s://%s:%d%s", scheme, host, port, path)
	req, _ := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, ""
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, body, resp.Header.Get("Content-Type")
}

// Try HTTP first, then HTTPS for the same path.
func httpGetEither(ctx context.Context, c *http.Client, host string, port int, path string, accept string) (scheme string, status int, body []byte, ctype string) {
	if s, b, ct := httpGet(ctx, c, "http", host, port, path, accept); s > 0 {
		return "http", s, b, ct
	}
	if s, b, ct := httpGet(ctx, c, "https", host, port, path, accept); s > 0 {
		return "https", s, b, ct
	}
	return "", 0, nil, ""
}

// Build a CatalogEntry skeleton from a discovered service+port+scheme+basePath.
//
// proxyPath is populated with a path the browser can hit directly. nginx
// forwards /discover-proxy/ to the sidecar's /proxy/ handler, which then
// reverse-proxies the request to the discovered upstream. This is what makes
// auto-discovered entries usable in the UI without operator action.
func makeEntry(svc Service, kind string, scheme string, port int, basePath string, label string, hints map[string]interface{}) CatalogEntry {
	if label == "" {
		label = fmt.Sprintf("%s (%s/%s)", svc.Name, svc.Namespace, kind)
	}
	if hints == nil {
		hints = map[string]interface{}{}
	}
	hints["_discoveredService"] = svc.FQDN
	hints["_discoveredPort"] = port
	hints["_discoveredScheme"] = scheme
	if basePath != "" {
		hints["_discoveredBasePath"] = basePath
	}
	base := strings.TrimSuffix(basePath, "/")
	proxyPath := fmt.Sprintf("/discover-proxy/%s/%s/%d%s/", scheme, svc.FQDN, port, base)
	return CatalogEntry{
		Name:       fmt.Sprintf("disc-%s-%s", svc.Namespace, svc.Name),
		Label:      label,
		Type:       kind,
		ProxyPath:  proxyPath,
		Discovered: true,
		Source:     "discovered",
		Hints:      hints,
	}
}

func probeElasticsearch(ctx context.Context, c *http.Client, svc Service, p ServicePort) (CatalogEntry, bool) {
	// ES/OpenSearch root returns JSON with "tagline" or cluster_name fields.
	scheme, status, body, _ := httpGetEither(ctx, c, svc.FQDN, p.Port, "/", "application/json")
	if status != 200 && status != 401 { // 401 still means "service is alive"
		return CatalogEntry{}, false
	}
	s := strings.ToLower(string(body))
	if !strings.Contains(s, "elasticsearch") && !strings.Contains(s, "opensearch") && !strings.Contains(s, "you know, for search") {
		return CatalogEntry{}, false
	}
	label := svc.Name
	if strings.Contains(s, "opensearch") {
		label += " (OpenSearch)"
	} else {
		label += " (Elasticsearch)"
	}
	return makeEntry(svc, "elasticsearch", scheme, p.Port, "", label, nil), true
}

func probePostgREST(ctx context.Context, c *http.Client, svc Service, p ServicePort) (CatalogEntry, bool) {
	scheme, status, body, _ := httpGetEither(ctx, c, svc.FQDN, p.Port, "/", "application/openapi+json")
	if status != 200 {
		return CatalogEntry{}, false
	}
	s := string(body)
	if !strings.Contains(s, `"swagger"`) && !strings.Contains(s, `"openapi"`) {
		return CatalogEntry{}, false
	}
	// PostgREST OpenAPI document carries either "postgrest" in info.title or
	// the characteristic "paths":{"/" pattern; check loosely.
	if !strings.Contains(strings.ToLower(s), "postgrest") && !strings.Contains(s, `"basePath":"/"`) {
		return CatalogEntry{}, false
	}
	// Surface as a generic REST entry — the browser already has a dedicated
	// /postgres/ path for the primary PG; secondary PGs go through the REST
	// adapter (it speaks OpenAPI + GET /<table> well enough for browse).
	return makeEntry(svc, "rest", scheme, p.Port, "", svc.Name+" (PostgREST)", map[string]interface{}{
		"flavor": "postgrest",
	}), true
}

func probeMinIO(ctx context.Context, c *http.Client, svc Service, p ServicePort) (CatalogEntry, bool) {
	// MinIO exposes /minio/health/live as 200 OK with empty body.
	_, status, _, _ := httpGetEither(ctx, c, svc.FQDN, p.Port, "/minio/health/live", "")
	if status != 200 {
		return CatalogEntry{}, false
	}
	// Without a known bucket we can't actually browse content, but the entry
	// surfaces so the operator can add a `hints.bucket: <name>` override
	// downstream. Default to root listing (works for some MinIO configs).
	scheme := "http"
	return makeEntry(svc, "minio", scheme, p.Port, "/", svc.Name+" (MinIO)", map[string]interface{}{
		"note": "Set hints.bucket via values.yaml override to scope to a single bucket.",
	}), true
}

func probeTAK(ctx context.Context, c *http.Client, svc Service, p ServicePort) (CatalogEntry, bool) {
	// TAK Server exposes /Marti/api/version → JSON {version: "..."} or text.
	scheme, status, body, _ := httpGetEither(ctx, c, svc.FQDN, p.Port, "/Marti/api/version", "application/json")
	if status != 200 && status != 401 {
		return CatalogEntry{}, false
	}
	s := string(body)
	if !strings.Contains(strings.ToLower(s), "tak") && !strings.Contains(s, "Marti") && status != 401 {
		// Some TAK deployments require client cert and answer 401; still a hit.
		if !looksLikeJSONObject(body) {
			return CatalogEntry{}, false
		}
	}
	return makeEntry(svc, "tak", scheme, p.Port, "", svc.Name+" (TAK Server)", map[string]interface{}{
		"apiBase": "Marti/api",
	}), true
}

func probeOGC(ctx context.Context, c *http.Client, svc Service, p ServicePort) (CatalogEntry, bool) {
	// Try GeoServer's canonical path first.
	for _, base := range []string{"/geoserver", "/ows", ""} {
		path := base + "/ows?service=WMS&version=1.3.0&request=GetCapabilities"
		scheme, status, body, _ := httpGetEither(ctx, c, svc.FQDN, p.Port, path, "application/xml")
		if status != 200 {
			continue
		}
		if !looksLikeOGCCapabilities(body) {
			continue
		}
		basePath := base
		if basePath == "" {
			basePath = "/"
		}
		hints := map[string]interface{}{
			"wfsPath": "ows",
			"wmsPath": "ows",
		}
		return makeEntry(svc, "ogc", scheme, p.Port, basePath, svc.Name+" (OGC)", hints), true
	}
	return CatalogEntry{}, false
}

func probeREST(ctx context.Context, c *http.Client, svc Service, p ServicePort) (CatalogEntry, bool) {
	scheme, status, body, ctype := httpGetEither(ctx, c, svc.FQDN, p.Port, "/", "application/json")
	if status == 0 {
		return CatalogEntry{}, false
	}
	if !strings.Contains(strings.ToLower(ctype), "json") && !looksLikeJSON(body) {
		return CatalogEntry{}, false
	}
	return makeEntry(svc, "rest", scheme, p.Port, "/", svc.Name+" (REST)", nil), true
}

// ─── Heuristics ───────────────────────────────────────────────────────────

func looksLikeJSON(b []byte) bool {
	s := strings.TrimSpace(string(b))
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}
func looksLikeJSONObject(b []byte) bool {
	s := strings.TrimSpace(string(b))
	return strings.HasPrefix(s, "{")
}
func looksLikeOGCCapabilities(b []byte) bool {
	// Tolerant XML sniff — accept any document whose root local-name contains
	// "Capabilities" (WMS_Capabilities, WFS_Capabilities, WMTS_Capabilities).
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if se, ok := tok.(xml.StartElement); ok {
			return strings.Contains(strings.ToLower(se.Name.Local), "capabilities")
		}
	}
}

// ─── Catalog assembly + serving ───────────────────────────────────────────

type Server struct {
	cfg       Config
	kube      *kubeClient
	probeC    *http.Client
	staticCat Catalog
	mu        sync.RWMutex
	current   Catalog // last good combined catalog
	// allowedTargets gates the /proxy/ handler. Key: "<scheme>|<host>|<port>".
	// Rebuilt on every refresh from both static and discovered entries to
	// prevent SSRF: only services the sidecar has explicitly catalogued can
	// be reached through the dynamic proxy.
	allowedTargets map[string]bool
	// takTargets is the subset of allowedTargets whose discovered/static type
	// is "tak". Requests to these targets use takTransport (mTLS) instead of
	// the plain proxyTransport.
	takTargets map[string]bool
	// proxyTransport is shared across all reverse-proxy requests so connection
	// pooling / keep-alives apply. TLS verification is disabled (intra-cluster,
	// self-signed certs are the norm).
	proxyTransport *http.Transport
	// takTransport is rebuilt whenever TAK creds change. nil until creds load.
	takTransport   *http.Transport
	takTransportMu sync.RWMutex
	tak            *TAKManager
	// takRuntimeEntry is the catalog entry for the admin-uploaded TAK server
	// address. Injected into every catalog refresh so allowedTargets/takTargets
	// stay correct. Guarded by mu.
	takRuntimeEntry *CatalogEntry
}

// buildRuntimeTAKEntry constructs a CatalogEntry for a TAK server specified
// by the operator at runtime (via the Admin panel). Uses the discover-proxy
// path so requests are routed through the sidecar's mTLS transport.
func buildRuntimeTAKEntry(host string, port int, scheme string) *CatalogEntry {
	proxyPath := fmt.Sprintf("/discover-proxy/%s/%s/%d/", scheme, host, port)
	return &CatalogEntry{
		Name:      "takserver-runtime",
		Label:     fmt.Sprintf("TAK Server (%s)", host),
		Type:      "tak",
		ProxyPath: proxyPath,
		Source:    "admin-upload",
		Hints: map[string]interface{}{
			"apiBase": "Marti/api",
			"service": host,
			"port":    port,
			"scheme":  scheme,
		},
	}
}

// injectRuntimeEntry merges takRuntimeEntry into entries: replaces an existing
// entry with the same name, or prepends if absent. Returns the updated slice.
func injectRuntimeEntry(entries []CatalogEntry, rte *CatalogEntry) []CatalogEntry {
	if rte == nil {
		return entries
	}
	for i, e := range entries {
		if e.Name == rte.Name {
			entries[i] = *rte
			return entries
		}
	}
	return append([]CatalogEntry{*rte}, entries...)
}

func (s *Server) loadStatic() {
	cat := Catalog{Version: 1, Source: "static", Entries: []CatalogEntry{}}
	if s.cfg.StaticPath == "" {
		s.staticCat = cat
		return
	}
	b, err := os.ReadFile(s.cfg.StaticPath)
	if err != nil {
		log.Printf("[discover] static catalog at %s not readable (%v); continuing with discovered-only", s.cfg.StaticPath, err)
		s.staticCat = cat
		return
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		log.Printf("[discover] static catalog parse error: %v; continuing with discovered-only", err)
		cat = Catalog{Version: 1, Source: "static", Entries: []CatalogEntry{}}
	}
	for i := range cat.Entries {
		cat.Entries[i].Source = "static"
	}
	s.staticCat = cat
	log.Printf("[discover] loaded %d static catalog entries", len(cat.Entries))
}

func (s *Server) refresh(ctx context.Context) {
	t0 := time.Now()

	// Without a kube client, we still publish the static catalog as the current
	// view so the UI doesn't get an empty/warming response forever.
	if s.kube == nil {
		s.mu.Lock()
		s.current = Catalog{
			Version:     1,
			Source:      "discover-sidecar (no-kube)",
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Entries:     s.staticCat.Entries,
		}
		s.mu.Unlock()
		return
	}

	// 1. List services in scope.
	svcs, err := s.kube.listServices(ctx)
	if err != nil {
		log.Printf("[discover] list services failed: %v", err)
		return
	}
	inScope := svcs[:0]
	for _, sv := range svcs {
		if nsMatches(sv.Namespace, s.cfg.NSIncludeGlob, s.cfg.NSExclude, s.cfg.OwnNamespace) {
			inScope = append(inScope, sv)
		}
	}

	// 2. Probe (or list-only) each service in parallel with bounded concurrency.
	type result struct {
		entry CatalogEntry
		ok    bool
	}
	results := make([]result, len(inScope))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := range inScope {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if !s.cfg.ProbeEnabled {
				results[i] = result{
					entry: makeEntry(inScope[i], "unknown", "http", firstPort(inScope[i].Ports), "/", inScope[i].Name+" (unknown)", nil),
					ok:    true,
				}
				return
			}
			pctx, cancel := context.WithTimeout(ctx, s.cfg.ProbeTimeout*time.Duration(len(probers)+1))
			defer cancel()
			ent, ok := probeService(pctx, s.probeC, inScope[i])
			if !ok {
				// Demote to "unknown" so operators see the Service exists.
				ent = makeEntry(inScope[i], "unknown", "http", firstPort(inScope[i].Ports), "/", inScope[i].Name+" (unknown)", nil)
				ok = true
			}
			results[i] = result{entry: ent, ok: ok}
		}(i)
	}
	wg.Wait()

	// 3. Merge: static catalog entries win on name collision.
	merged := Catalog{
		Version:     1,
		Source:      "discover-sidecar",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Entries:     make([]CatalogEntry, 0, len(results)+len(s.staticCat.Entries)),
	}
	seen := map[string]bool{}
	for _, e := range s.staticCat.Entries {
		merged.Entries = append(merged.Entries, e)
		seen[e.Name] = true
	}
	for _, r := range results {
		if !r.ok {
			continue
		}
		// Skip when a static entry already covers the same FQDN+port.
		if shadowsStatic(r.entry, s.staticCat.Entries) {
			continue
		}
		// Deduplicate by name in case of unusual k8s name collisions.
		nm := r.entry.Name
		i := 2
		for seen[nm] {
			nm = fmt.Sprintf("%s-%d", r.entry.Name, i)
			i++
		}
		r.entry.Name = nm
		seen[nm] = true
		merged.Entries = append(merged.Entries, r.entry)
	}
	sort.SliceStable(merged.Entries, func(i, j int) bool {
		// Static first, then discovered. Within each, by label.
		if merged.Entries[i].Discovered != merged.Entries[j].Discovered {
			return !merged.Entries[i].Discovered
		}
		return strings.ToLower(merged.Entries[i].Label) < strings.ToLower(merged.Entries[j].Label)
	})

	// Inject admin-uploaded runtime TAK entry (takes precedence over the
	// baked-in static catalog entry so the operator's choice always wins).
	s.mu.RLock()
	rte := s.takRuntimeEntry
	s.mu.RUnlock()
	merged.Entries = injectRuntimeEntry(merged.Entries, rte)

	s.mu.Lock()
	s.current = merged
	s.allowedTargets = buildAllowedTargets(merged.Entries)
	s.takTargets = buildTAKTargets(merged.Entries)
	s.mu.Unlock()
	// refresh. Rebuilds takTransport so new outbound connections use the new
	// cert without a pod restart.
	if s.tak != nil {
		if creds, err := s.tak.loadIfChanged(); err == nil && creds != nil {
			s.rebuildTAKTransport()
		}
	}

	log.Printf("[discover] refresh: %d in-scope services, %d static, %d discovered → %d total, %d proxy targets (%d TAK) (%.0fms)",
		len(inScope), len(s.staticCat.Entries), len(merged.Entries)-len(s.staticCat.Entries), len(merged.Entries), len(s.allowedTargets), len(s.takTargets), float64(time.Since(t0).Milliseconds()))
}

func firstPort(ps []ServicePort) int {
	if len(ps) == 0 {
		return 80
	}
	return ps[0].Port
}

// shadowsStatic returns true when a static entry already points at the same
// in-cluster service+port as a discovered candidate.
func shadowsStatic(disc CatalogEntry, static []CatalogEntry) bool {
	dFQDN, _ := disc.Hints["_discoveredService"].(string)
	dPort, _ := disc.Hints["_discoveredPort"].(float64) // JSON numbers
	if dPortInt, ok := disc.Hints["_discoveredPort"].(int); ok {
		dPort = float64(dPortInt)
	}
	for _, s := range static {
		svc, _ := s.Hints["service"].(string)
		port, _ := s.Hints["port"].(float64)
		if portInt, ok := s.Hints["port"].(int); ok {
			port = float64(portInt)
		}
		if svc != "" && svc == dFQDN && (port == 0 || port == dPort) {
			return true
		}
	}
	return false
}

// ─── HTTP server ──────────────────────────────────────────────────────────

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cat := s.current
	s.mu.RUnlock()
	if cat.Version == 0 {
		// Pre-first-refresh: return static-only so the UI gets something usable.
		s.mu.RLock()
		cat = Catalog{
			Version:     1,
			Source:      "discover-sidecar (warming)",
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			Entries:     s.staticCat.Entries,
		}
		s.mu.RUnlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(cat)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.current.Version != 0
	s.mu.RUnlock()
	if !ready {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("warming\n"))
		return
	}
	w.WriteHeader(200)
	_, _ = w.Write([]byte("ready\n"))
}

// ─── Dynamic reverse proxy for discovered services ────────────────────────
//
// URL shape (from the browser via nginx):
//   /discover-proxy/<scheme>/<host>/<port>/<rest>?<query>
// nginx strips the /discover-proxy/ prefix and forwards to:
//   http://127.0.0.1:9090/proxy/<scheme>/<host>/<port>/<rest>?<query>
// This handler parses the path, validates the target against the discovery
// allowlist (SSRF guard), then reverse-proxies the request to:
//   <scheme>://<host>:<port>/<rest>?<query>
//
// Only http/https are accepted. The body, method, and query string are
// preserved untouched (httputil.ReverseProxy handles streaming and
// hop-by-hop header stripping).

// buildAllowedTargets distills the merged catalog into a set of
// "<scheme>|<host>|<port>" keys that the /proxy/ handler will accept.
// Static entries contribute their hints.service/hints.port/hints.scheme;
// discovered entries contribute the values stashed by makeEntry().
func buildAllowedTargets(entries []CatalogEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		var host, scheme string
		var port int
		if v, ok := e.Hints["_discoveredService"].(string); ok {
			host = v
		}
		if v, ok := e.Hints["_discoveredScheme"].(string); ok {
			scheme = v
		}
		if v, ok := e.Hints["_discoveredPort"].(int); ok {
			port = v
		} else if v, ok := e.Hints["_discoveredPort"].(float64); ok {
			port = int(v)
		}
		if host == "" {
			if v, ok := e.Hints["service"].(string); ok {
				host = v
			}
		}
		if scheme == "" {
			if v, ok := e.Hints["scheme"].(string); ok {
				scheme = v
			}
		}
		if port == 0 {
			if v, ok := e.Hints["port"].(int); ok {
				port = v
			} else if v, ok := e.Hints["port"].(float64); ok {
				port = int(v)
			}
		}
		if scheme == "" {
			scheme = "http"
		}
		if host == "" || port == 0 {
			continue
		}
		if scheme != "http" && scheme != "https" {
			continue
		}
		out[scheme+"|"+host+"|"+strconv.Itoa(port)] = true
	}
	return out
}

func (s *Server) targetAllowed(scheme, host string, port int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowedTargets[scheme+"|"+host+"|"+strconv.Itoa(port)]
}

func (s *Server) targetIsTAK(scheme, host string, port int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.takTargets[scheme+"|"+host+"|"+strconv.Itoa(port)]
}

// buildTAKTargets returns the subset of allowed targets whose type is "tak".
// Used by handleProxy to choose the mTLS transport for TAK-bound requests.
func buildTAKTargets(entries []CatalogEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		if strings.ToLower(e.Type) != "tak" {
			continue
		}
		var host, scheme string
		var port int
		if v, ok := e.Hints["_discoveredService"].(string); ok {
			host = v
		}
		if v, ok := e.Hints["_discoveredScheme"].(string); ok {
			scheme = v
		}
		if v, ok := e.Hints["_discoveredPort"].(int); ok {
			port = v
		} else if v, ok := e.Hints["_discoveredPort"].(float64); ok {
			port = int(v)
		}
		if host == "" {
			if v, ok := e.Hints["service"].(string); ok {
				host = v
			}
		}
		if scheme == "" {
			if v, ok := e.Hints["scheme"].(string); ok {
				scheme = v
			}
		}
		if port == 0 {
			if v, ok := e.Hints["port"].(int); ok {
				port = v
			} else if v, ok := e.Hints["port"].(float64); ok {
				port = int(v)
			}
		}
		if scheme == "" {
			scheme = "https"
		}
		if host == "" || port == 0 {
			continue
		}
		out[scheme+"|"+host+"|"+strconv.Itoa(port)] = true
	}
	return out
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Strip the "/proxy/" prefix and split into scheme/host/port/rest.
	p := strings.TrimPrefix(r.URL.Path, "/proxy/")
	parts := strings.SplitN(p, "/", 4)
	if len(parts) < 3 {
		http.Error(w, "discover-proxy: expected /<scheme>/<host>/<port>/<path>", http.StatusBadRequest)
		return
	}
	scheme, host, portStr := parts[0], parts[1], parts[2]
	rest := ""
	if len(parts) == 4 {
		rest = parts[3]
	}
	if scheme != "http" && scheme != "https" {
		http.Error(w, "discover-proxy: scheme must be http or https", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "discover-proxy: invalid port", http.StatusBadRequest)
		return
	}
	if host == "" || strings.ContainsAny(host, " \t\r\n") {
		http.Error(w, "discover-proxy: invalid host", http.StatusBadRequest)
		return
	}
	if !s.targetAllowed(scheme, host, port) {
		http.Error(w, "discover-proxy: target not in discovery catalog", http.StatusForbidden)
		return
	}

	targetURL := &url.URL{
		Scheme: scheme,
		Host:   host + ":" + portStr,
	}
	// Pick transport: TAK targets get the mTLS-enabled transport (when creds
	// are loaded); everyone else uses the plain InsecureSkipVerify transport.
	var tr http.RoundTripper = s.proxyTransport
	if s.targetIsTAK(scheme, host, port) {
		if takTr := s.currentTAKTransport(); takTr != nil {
			tr = takTr
		}
	}
	rp := &httputil.ReverseProxy{
		Transport: tr,
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = "/" + rest
			// RawQuery preserved by ReverseProxy's caller (we don't touch it).
			req.Host = host // upstream sees its own hostname (good for vhost / SNI)
			// Strip any inbound Authorization we don't intend to forward upstream.
			// In ISAK the browser is talking to nginx (same origin) so there is
			// rarely an Authorization header; keep this defensive.
			req.Header.Del("Authorization")
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, e error) {
			log.Printf("[discover-proxy] upstream error %s://%s: %v", scheme, targetURL.Host, e)
			http.Error(rw, "discover-proxy: upstream unreachable: "+e.Error(), http.StatusBadGateway)
		},
	}
	// Expose response headers the UI relies on (Content-Range for paging, etc.).
	rp.ModifyResponse = func(resp *http.Response) error {
		if existing := resp.Header.Get("Access-Control-Expose-Headers"); existing == "" {
			resp.Header.Set("Access-Control-Expose-Headers", "Content-Range, Content-Length, ETag, Last-Modified, Content-Location")
		}
		return nil
	}
	rp.ServeHTTP(w, r)
}

// currentTAKTransport returns the shared mTLS transport, or nil if no TAK
// creds are loaded (caller should fall back to the plain transport).
func (s *Server) currentTAKTransport() *http.Transport {
	s.takTransportMu.RLock()
	defer s.takTransportMu.RUnlock()
	return s.takTransport
}

// rebuildTAKTransport regenerates s.takTransport from the current TAKManager
// state. Safe to call any time creds change; old transport is dropped on the
// floor (Go runtime closes its idle conns when GC reaches it).
func (s *Server) rebuildTAKTransport() {
	if s.tak == nil || !s.tak.loaded() {
		return
	}
	tlsCfg := s.tak.tlsConfig()
	tr := &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		Proxy:                 nil,
	}
	s.takTransportMu.Lock()
	s.takTransport = tr
	s.takTransportMu.Unlock()
}

// ─── Admin endpoints (TAK cert upload + status) ──────────────────────────
//
// All /admin/* endpoints require the operator-provided token in
//   X-DaVi-Admin-Token: <token>
// The token lives in a chart-managed K8s Secret; operator fetches it once via
//   kubectl get secret <release>-davi-admin-token -o jsonpath='{.data.token}' | base64 -d
// and pastes into the DaVi UI.
//
// /admin/tak-cert  (POST multipart/form-data)
//   Fields:
//     client_p12              file (required if no client_crt)
//     client_passphrase       text (optional)
//     truststore_p12          file (optional)
//     truststore_passphrase   text (optional)
//     client_crt              file (alt to client_p12, PEM)
//     client_key              file (alt to client_p12, PEM)
//     ca_crt                  file (alt to truststore_p12, PEM)
//   Parses + validates the cert in-memory, plants it as the active cert
//   immediately, then PATCHes the configured Secret so the cert survives a
//   pod restart. Returns 200 + status JSON on success.
//
// /admin/tak-cert/status (GET)
//   Returns whether a cert is loaded plus subject/expiry. Never returns key
//   material.

func (s *Server) requireAdminToken(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		http.Error(w, "admin disabled (no DAVI_ADMIN_TOKEN configured)", http.StatusForbidden)
		return false
	}
	tok := r.Header.Get("X-DaVi-Admin-Token")
	if tok == "" {
		http.Error(w, "missing X-DaVi-Admin-Token", http.StatusUnauthorized)
		return false
	}
	// Constant-time-ish comparison: stdlib avoids importing subtle here, but
	// the token has high entropy (48 chars) so timing-side-channel risk is
	// negligible against an unauthenticated remote attacker.
	if len(tok) != len(s.cfg.AdminToken) || tok != s.cfg.AdminToken {
		http.Error(w, "invalid admin token", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) handleAdminTAKStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminToken(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"loaded":     false,
		"secretName": s.cfg.TAKSecretName,
		"mountPath":  s.cfg.TAKMountPath,
		"takTargets": s.takTargetCount(),
	}
	if creds, ok := s.tak.status(); ok {
		resp["loaded"] = true
		resp["sourceKind"] = creds.SourceKind
		resp["subject"] = creds.Subject
		resp["issuer"] = creds.Issuer
		if !creds.NotBefore.IsZero() {
			resp["notBefore"] = creds.NotBefore.UTC().Format(time.RFC3339)
		}
		if !creds.NotAfter.IsZero() {
			resp["notAfter"] = creds.NotAfter.UTC().Format(time.RFC3339)
			resp["daysUntilExpiry"] = int(time.Until(creds.NotAfter).Hours() / 24)
		}
		resp["loadedAt"] = creds.LoadedAt.UTC().Format(time.RFC3339)
		resp["trustStoreCAs"] = creds.CAs != nil
		if creds.TruststoreWarning != "" {
			resp["truststoreWarning"] = creds.TruststoreWarning
		}
	}
	// Include the currently configured TAK server address so the UI can
	// pre-populate the fields on re-open.
	s.mu.RLock()
	rte := s.takRuntimeEntry
	s.mu.RUnlock()
	if rte != nil {
		resp["takServer"] = map[string]interface{}{
			"host":   rte.Hints["service"],
			"port":   rte.Hints["port"],
			"scheme": rte.Hints["scheme"],
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) takTargetCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.takTargets)
}

func (s *Server) handleAdminTAKUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminToken(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	// Limit upload size to 256 KiB; TAK certs are small (~4-8 KB).
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	if err := r.ParseMultipartForm(64 * 1024); err != nil {
		http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}

	fields := map[string][]byte{}
	picks := []string{"client_p12", "truststore_p12", "client_crt", "client_key", "ca_crt"}
	for _, name := range picks {
		if f, _, err := r.FormFile(name); err == nil {
			defer f.Close()
			b, _ := io.ReadAll(io.LimitReader(f, 256*1024))
			if len(b) > 0 {
				fields[name] = b
			}
		}
	}
	if v := strings.TrimSpace(r.FormValue("client_passphrase")); v != "" {
		fields["client.passphrase"] = []byte(v)
	}
	if v := strings.TrimSpace(r.FormValue("truststore_passphrase")); v != "" {
		fields["truststore.passphrase"] = []byte(v)
	}

	// TAK server address (optional — operator can set on first upload or update
	// later without re-uploading the cert by uploading the same cert again with
	// new address fields).
	takHost := strings.TrimSpace(r.FormValue("tak_host"))
	takScheme := strings.TrimSpace(r.FormValue("tak_scheme"))
	if takScheme == "" {
		takScheme = "https"
	}
	takPort := 8443
	if p, err := strconv.Atoi(strings.TrimSpace(r.FormValue("tak_port"))); err == nil && p > 0 {
		takPort = p
	}
	if takHost != "" {
		fields["tak.host"] = []byte(takHost)
		fields["tak.port"] = []byte(strconv.Itoa(takPort))
		fields["tak.scheme"] = []byte(takScheme)
	}

	// Translate multipart field names to the on-disk filenames the loader
	// expects.
	disk := map[string][]byte{}
	name := map[string]string{
		"client_p12":     "client.p12",
		"truststore_p12": "truststore.p12",
		"client_crt":     "client.crt",
		"client_key":     "client.key",
		"ca_crt":         "ca.crt",
	}
	for k, v := range fields {
		if out, ok := name[k]; ok {
			disk[out] = v
		} else {
			disk[k] = v
		}
	}

	// Sanity: we need a client cert in one form or the other.
	if _, hasP12 := disk["client.p12"]; !hasP12 {
		_, hasCrt := disk["client.crt"]
		_, hasKey := disk["client.key"]
		if !(hasCrt && hasKey) {
			http.Error(w, "need client_p12 OR (client_crt + client_key)", http.StatusBadRequest)
			return
		}
	}

	// Validate by parsing in-memory before we persist anything.
	creds, err := parseTAKFromMemory(disk)
	if err != nil {
		http.Error(w, "validate cert: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Persist to the Secret first so a crash mid-upload leaves us consistent.
	if s.cfg.TAKSecretName != "" && s.kube != nil {
		if err := s.kube.patchSecretData(r.Context(), s.cfg.OwnNamespace, s.cfg.TAKSecretName, disk); err != nil {
			http.Error(w, "persist to Secret: "+err.Error(), http.StatusBadGateway)
			return
		}
	}

	// Plant in-memory so new requests use the cert immediately.
	s.tak.setFromMemory(creds)
	s.rebuildTAKTransport()

	// If the operator specified a TAK server address, build a runtime catalog
	// entry and inject it into the allow-list immediately (no wait for refresh).
	if takHost != "" {
		rte := buildRuntimeTAKEntry(takHost, takPort, takScheme)
		s.mu.Lock()
		s.takRuntimeEntry = rte
		curEntries := injectRuntimeEntry(append([]CatalogEntry(nil), s.current.Entries...), rte)
		s.allowedTargets = buildAllowedTargets(curEntries)
		s.takTargets = buildTAKTargets(curEntries)
		s.mu.Unlock()
		log.Printf("[admin] TAK server set: %s://%s:%d", takScheme, takHost, takPort)
	}

	log.Printf("[admin] TAK cert uploaded: subject=%q expiresIn=%s persisted=%v",
		creds.Subject, time.Until(creds.NotAfter).Truncate(time.Hour), s.cfg.TAKSecretName != "")

	s.handleAdminTAKStatus(w, r)
}

// parseTAKFromMemory accepts the in-flight payload keyed by on-disk filenames
// and runs the same loader the disk path uses.
func parseTAKFromMemory(disk map[string][]byte) (*TAKCreds, error) {
	has := func(name string) bool { _, ok := disk[name]; return ok }
	read := func(name string) ([]byte, error) {
		b, ok := disk[name]
		if !ok {
			return nil, os.ErrNotExist
		}
		return b, nil
	}
	switch {
	case has("client.crt") && has("client.key"):
		return loadPEM(read, has)
	case has("client.p12"):
		return loadP12(read, has)
	default:
		return nil, errors.New("no client cert material in upload")
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg := loadConfig()
	log.Printf("[discover] starting davi-discover; port=%s refresh=%s probeTimeout=%s nsIncludes=%v nsExclude=%v probe=%v",
		cfg.Port, cfg.RefreshInterval, cfg.ProbeTimeout, cfg.NSIncludeGlob, cfg.NSExclude, cfg.ProbeEnabled)

	kube, err := newKubeClient(10 * time.Second)
	if err != nil {
		log.Printf("[discover] kube client setup failed (%v); serving static-only", err)
		kube = nil
	}

	// Probe HTTP client — intra-cluster, accept self-signed.
	probeTr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{
			Timeout:   cfg.ProbeTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		Proxy: nil,
	}
	probeC := &http.Client{
		Transport: probeTr,
		Timeout:   cfg.ProbeTimeout + 500*time.Millisecond,
	}

	srv := &Server{cfg: cfg, kube: kube, probeC: probeC}
	srv.loadStatic()
	srv.tak = newTAKManager(cfg.TAKMountPath)

	// Best-effort initial TAK load; missing creds are fine (admin upload
	// path can populate later).
	if creds, err := srv.tak.loadIfChanged(); err != nil {
		log.Printf("[discover] TAK creds not loaded at startup (%v); /admin/tak-cert can supply them", err)
	} else if creds != nil {
		log.Printf("[discover] TAK creds loaded (%s) subject=%q expires=%s",
			creds.SourceKind, creds.Subject, creds.NotAfter.Format(time.RFC3339))
	}

	// Restore admin-uploaded TAK server address from the Secret mount so the
	// runtime catalog entry survives pod restarts without operator re-entry.
	if tgt := loadTAKTargetFrom(cfg.TAKMountPath); tgt.Host != "" {
		srv.takRuntimeEntry = buildRuntimeTAKEntry(tgt.Host, tgt.Port, tgt.Scheme)
		log.Printf("[discover] TAK server restored from mount: %s://%s:%d", tgt.Scheme, tgt.Host, tgt.Port)
	}

	// Dedicated transport for the dynamic reverse proxy. Self-signed certs are
	// the norm intra-cluster, so verification is off; this transport is only
	// ever used for hosts in srv.allowedTargets.
	srv.proxyTransport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		Proxy:                 nil,
	}
	srv.rebuildTAKTransport()

	// Initial refresh in the background; HTTP server is up immediately and
	// serves static-only until the first cycle completes.
	go func() {
		ticker := time.NewTicker(cfg.RefreshInterval)
		defer ticker.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		srv.refresh(ctx)
		cancel()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			srv.refresh(ctx)
			cancel()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/static.json", srv.handleStatic)
	mux.HandleFunc("/healthz", srv.handleHealthz)
	mux.HandleFunc("/readyz", srv.handleReady)
	mux.HandleFunc("/proxy/", srv.handleProxy)
	mux.HandleFunc("/admin/tak-cert/status", srv.handleAdminTAKStatus)
	mux.HandleFunc("/admin/tak-cert", srv.handleAdminTAKUpload)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("[discover] listening on :%s", cfg.Port)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[discover] http server: %v", err)
	}
}
