// tak.go — TAK Server mTLS credential loader for the discovery sidecar.
//
// TAK Server uses mutual TLS for its Marti API: the client must present an
// operator-issued certificate and (typically) verify the server using a CA
// chain shipped in a separate "truststore". TAK normally hands operators two
// PKCS#12 files (`<user>.p12` + `truststore-root.p12`) each with a passphrase,
// but PEM-split material works just as well.
//
// This module:
//   - Auto-detects PEM or PKCS#12 layout at a mount path.
//   - Returns a *tls.Config ready to plug into an http.Transport.
//   - Reloads when the mount changes (kubelet re-projects updated Secrets
//     within ~60s, so cert rotation works without a pod restart).
//
// Files recognised at the mount path (all optional unless paired):
//
//	PEM:    client.crt + client.key (+ optional ca.crt)
//	P12:    client.p12 (+ optional client.passphrase)
//	        truststore.p12 (+ optional truststore.passphrase)
//
// Passphrase files are stripped of trailing whitespace before use. Empty
// passphrase is supported (operator can omit the *.passphrase file).
//
// Refuses to enable mTLS silently: if neither layout is present, the manager
// reports "not loaded" and callers fall back to InsecureSkipVerify.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

const defaultTAKMountPath = "/etc/davi-discover/tak"

// TAKCreds carries everything we need to build a TLS config plus a handful
// of metadata fields for the /admin/tak-cert/status endpoint. Private key
// material is never serialised to JSON (no struct tags).
type TAKCreds struct {
	Cert              tls.Certificate
	CAs               *x509.CertPool
	Subject           string
	Issuer            string
	NotBefore         time.Time
	NotAfter          time.Time
	SourceKind        string // "pem" | "pkcs12"
	LoadedAt          time.Time
	TruststoreWarning string // non-empty when truststore passphrase failed and InsecureSkipVerify is active
}

// TAKManager owns the cert lifecycle. Safe for concurrent use.
type TAKManager struct {
	mu        sync.RWMutex
	mountPath string
	current   *TAKCreds
	lastMtime time.Time
}

func newTAKManager(mountPath string) *TAKManager {
	if mountPath == "" {
		mountPath = defaultTAKMountPath
	}
	return &TAKManager{mountPath: mountPath}
}

// loadIfChanged inspects the mount for newer mtimes and reloads when needed.
// Called once per refresh tick; cheap when nothing changed.
func (m *TAKManager) loadIfChanged() (*TAKCreds, error) {
	latest := latestMTime(m.mountPath)
	m.mu.RLock()
	if m.current != nil && !latest.IsZero() && latest.Equal(m.lastMtime) {
		c := m.current
		m.mu.RUnlock()
		return c, nil
	}
	m.mu.RUnlock()
	creds, err := loadTAKCredsFrom(m.mountPath)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.current = creds
	m.lastMtime = latest
	m.mu.Unlock()
	return creds, nil
}

// setFromMemory plants creds parsed from an in-process upload (admin endpoint)
// so they're effective immediately, without waiting for kubelet to re-project
// the mounted Secret. The Secret is updated in parallel for persistence.
func (m *TAKManager) setFromMemory(creds *TAKCreds) {
	m.mu.Lock()
	m.current = creds
	// Bump lastMtime so loadIfChanged() doesn't re-read stale on-disk files
	// before the kubelet re-projects.
	m.lastMtime = time.Now()
	m.mu.Unlock()
}

func (m *TAKManager) status() (*TAKCreds, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return nil, false
	}
	c := *m.current
	return &c, true
}

// tlsConfig returns the outbound TLS config for TAK targets. Falls back to
// InsecureSkipVerify when no creds are loaded so the existing prober flow
// (which already tolerates 401s) keeps working before the operator uploads.
func (m *TAKManager) tlsConfig() *tls.Config {
	creds, ok := m.status()
	if !ok {
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{creds.Cert},
		MinVersion:   tls.VersionTLS12,
	}
	if creds.CAs != nil {
		cfg.RootCAs = creds.CAs
	} else {
		// Cert is loaded but the operator did not provide a truststore;
		// upstream verification is impossible, so skip-verify.
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

// loaded reports whether the manager currently holds a usable cert.
func (m *TAKManager) loaded() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current != nil
}

// latestMTime returns the newest mtime among files in dir, or zero if dir is
// missing/empty.
func latestMTime(dir string) time.Time {
	var latest time.Time
	entries, err := os.ReadDir(dir)
	if err != nil {
		return latest
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}

// loadTAKCredsFrom auto-detects PEM vs PKCS#12 layout and returns parsed creds.
func loadTAKCredsFrom(mountPath string) (*TAKCreds, error) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(mountPath, name))
		return err == nil
	}
	read := func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(mountPath, name))
	}

	switch {
	case has("client.crt") && has("client.key"):
		return loadPEM(read, has)
	case has("client.p12"):
		return loadP12(read, has)
	default:
		return nil, errors.New("no TAK creds at " + mountPath +
			" (expected client.crt+client.key OR client.p12)")
	}
}

func loadPEM(read func(string) ([]byte, error), has func(string) bool) (*TAKCreds, error) {
	crtPEM, err := read("client.crt")
	if err != nil {
		return nil, fmt.Errorf("read client.crt: %w", err)
	}
	keyPEM, err := read("client.key")
	if err != nil {
		return nil, fmt.Errorf("read client.key: %w", err)
	}
	cert, err := tls.X509KeyPair(crtPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse PEM keypair: %w", err)
	}
	var cas *x509.CertPool
	if has("ca.crt") {
		caPEM, err := read("ca.crt")
		if err == nil && len(caPEM) > 0 {
			cas = x509.NewCertPool()
			if !cas.AppendCertsFromPEM(caPEM) {
				return nil, errors.New("ca.crt did not contain any parseable certificates")
			}
		}
	}
	return finalize(cert, cas, "pem"), nil
}

func loadP12(read func(string) ([]byte, error), has func(string) bool) (*TAKCreds, error) {
	p12Bytes, err := read("client.p12")
	if err != nil {
		return nil, fmt.Errorf("read client.p12: %w", err)
	}
	pass := readPassphrase(read, "client.passphrase")
	cert, err := parseClientP12(p12Bytes, pass)
	if err != nil {
		// Try empty passphrase as a fallback (operator omitted the file).
		if pass != "" {
			if c2, e2 := parseClientP12(p12Bytes, ""); e2 == nil {
				cert = c2
				err = nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("decode client.p12: %w", err)
		}
	}

	var cas *x509.CertPool
	var truststoreWarning string
	if has("truststore.p12") {
		tsBytes, err := read("truststore.p12")
		if err == nil && len(tsBytes) > 0 {
			tsPass := readPassphrase(read, "truststore.passphrase")
			pool, perr := parseTruststoreP12(tsBytes, tsPass)
			// Fallback 1: empty passphrase (operator omitted the *.passphrase file).
			if perr != nil && tsPass != "" {
				pool, perr = parseTruststoreP12(tsBytes, "")
			}
			// Fallback 2: TAK Server default passphrase used in many generated bundles.
			if perr != nil && tsPass != "atakatak" {
				pool, perr = parseTruststoreP12(tsBytes, "atakatak")
			}
			if perr != nil {
				// Soft-fail: the truststore passphrase is wrong but the client
				// cert parsed fine.  Proceed with cas=nil so tlsConfig() uses
				// InsecureSkipVerify for server-cert verification while still
				// presenting the client cert.  A warning is included in the
				// status JSON so the operator knows verification is degraded.
				truststoreWarning = "truststore.p12 passphrase incorrect – server cert verification disabled (InsecureSkipVerify active)"
				log.Printf("[tak] %s: %v", truststoreWarning, perr)
			} else {
				cas = pool
			}
		}
	}
	creds := finalize(cert, cas, "pkcs12")
	creds.TruststoreWarning = truststoreWarning
	return creds, nil
}

func parseClientP12(p12Bytes []byte, pass string) (tls.Certificate, error) {
	key, leaf, chain, err := pkcs12.DecodeChain(p12Bytes, pass)
	if err != nil {
		return tls.Certificate{}, err
	}
	if leaf == nil {
		return tls.Certificate{}, errors.New("client.p12 contained no leaf certificate")
	}
	cert := tls.Certificate{
		PrivateKey: key,
		Leaf:       leaf,
	}
	cert.Certificate = append(cert.Certificate, leaf.Raw)
	for _, c := range chain {
		cert.Certificate = append(cert.Certificate, c.Raw)
	}
	return cert, nil
}

func parseTruststoreP12(p12Bytes []byte, pass string) (*x509.CertPool, error) {
	certs, err := pkcs12.DecodeTrustStore(p12Bytes, pass)
	if err != nil {
		// Some truststores were generated as ordinary PKCS#12 keystores
		// (with placeholder keys); fall back to DecodeChain and harvest certs.
		_, leaf, chain, e2 := pkcs12.DecodeChain(p12Bytes, pass)
		if e2 != nil {
			return nil, err // surface the original DecodeTrustStore error
		}
		if leaf != nil {
			certs = append(certs, leaf)
		}
		certs = append(certs, chain...)
	}
	if len(certs) == 0 {
		return nil, errors.New("truststore contained no certificates")
	}
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	return pool, nil
}

func finalize(cert tls.Certificate, cas *x509.CertPool, kind string) *TAKCreds {
	var subj, issuer string
	var notBefore, notAfter time.Time
	if cert.Leaf == nil && len(cert.Certificate) > 0 {
		if x, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			cert.Leaf = x
		}
	}
	if cert.Leaf != nil {
		subj = cert.Leaf.Subject.String()
		issuer = cert.Leaf.Issuer.String()
		notBefore = cert.Leaf.NotBefore
		notAfter = cert.Leaf.NotAfter
	}
	return &TAKCreds{
		Cert:       cert,
		CAs:        cas,
		Subject:    subj,
		Issuer:     issuer,
		NotBefore:  notBefore,
		NotAfter:   notAfter,
		SourceKind: kind,
		LoadedAt:   time.Now(),
	}
}

func readPassphrase(read func(string) ([]byte, error), name string) string {
	b, err := read(name)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), " \t\r\n")
}

// TAKTarget holds the operator-specified TAK Server address (separate from
// the mTLS credentials). Persisted alongside the cert in the Secret mount so
// it survives pod restarts without re-entry.
type TAKTarget struct {
	Host   string
	Port   int
	Scheme string
}

// loadTAKTargetFrom reads tak.host / tak.port / tak.scheme from mountPath.
// Returns a zero-value TAKTarget (Host=="") when no address has been saved.
func loadTAKTargetFrom(mountPath string) TAKTarget {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(mountPath, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	host := read("tak.host")
	if host == "" {
		return TAKTarget{}
	}
	port := 8443
	if p, err := strconv.Atoi(read("tak.port")); err == nil && p > 0 {
		port = p
	}
	scheme := read("tak.scheme")
	if scheme == "" {
		scheme = "https"
	}
	return TAKTarget{Host: host, Port: port, Scheme: scheme}
}
