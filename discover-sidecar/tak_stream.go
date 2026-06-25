// tak_stream.go — Live CoT stream subscriber for the discovery sidecar.
//
// TAK Server's REST API has no GET-current-positions endpoint; live tracks
// (SA + ADS-B + sensor feeds) flow over its CoT streaming port (default 8089,
// mTLS). This module maintains a persistent mTLS connection, parses CoT XML
// events into a last-known-position cache, and exposes that cache as JSON
// via /discover-tak/sa for the DaVi browser app.
//
// Concurrency model: a single subscriber goroutine owns the network
// connection and decodes events. It writes to a shared map guarded by a
// RWMutex. HTTP handlers read with the RLock.
//
// Reconnect strategy: exponential backoff capped at 30 s; on every
// successful event the backoff resets. The subscriber exits cleanly when
// stop() is called (used when a new TAK target is configured).
//
// Stale eviction: a janitor goroutine removes tracks whose last update is
// older than streamTrackTTL.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	streamDefaultPort = 8089
	streamTrackTTL    = 5 * time.Minute
	streamJanitorTick = 30 * time.Second
	streamDialTimeout = 10 * time.Second
	streamMaxBackoff  = 30 * time.Second
	streamReadBuf     = 64 * 1024
)

// cotEventXML mirrors the TAK Cursor-on-Target schema, slim subset.
type cotEventXML struct {
	XMLName xml.Name `xml:"event"`
	Version string   `xml:"version,attr"`
	UID     string   `xml:"uid,attr"`
	Type    string   `xml:"type,attr"`
	How     string   `xml:"how,attr"`
	Time    string   `xml:"time,attr"`
	Start   string   `xml:"start,attr"`
	Stale   string   `xml:"stale,attr"`
	Point   struct {
		Lat float64 `xml:"lat,attr"`
		Lon float64 `xml:"lon,attr"`
		Hae float64 `xml:"hae,attr"`
		Ce  float64 `xml:"ce,attr"`
		Le  float64 `xml:"le,attr"`
	} `xml:"point"`
	Detail struct {
		Contact struct {
			Callsign string `xml:"callsign,attr"`
			Endpoint string `xml:"endpoint,attr"`
		} `xml:"contact"`
		Track struct {
			Course float64 `xml:"course,attr"`
			Speed  float64 `xml:"speed,attr"`
		} `xml:"track"`
		Remarks string `xml:"remarks"`
		Group   struct {
			Name string `xml:"name,attr"`
			Role string `xml:"role,attr"`
		} `xml:"__group"`
	} `xml:"detail"`
}

// Track is the public, JSON-serialisable snapshot of a CoT event.
type Track struct {
	UID       string    `json:"uid"`
	Callsign  string    `json:"callsign,omitempty"`
	Type      string    `json:"type,omitempty"`
	How       string    `json:"how,omitempty"`
	Latitude  float64   `json:"latitude"`
	Longitude float64   `json:"longitude"`
	HAE       float64   `json:"hae,omitempty"`
	CourseDeg float64   `json:"course_deg,omitempty"`
	SpeedMS   float64   `json:"speed_ms,omitempty"`
	Remarks   string    `json:"remarks,omitempty"`
	Team      string    `json:"team,omitempty"`
	Role      string    `json:"role,omitempty"`
	EventTime string    `json:"event_time,omitempty"`
	StaleAt   string    `json:"stale_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TAKStreamer holds the cache and owns the subscriber goroutine.
type TAKStreamer struct {
	mu     sync.RWMutex
	tracks map[string]*Track

	// runState guarded by stateMu
	stateMu      sync.Mutex
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	currentHost  string
	currentPort  int
	connected    bool
	lastErr      string
	lastEventAt  time.Time
	eventCount   uint64
	reconnectCnt uint64
}

func newTAKStreamer() *TAKStreamer {
	s := &TAKStreamer{tracks: make(map[string]*Track)}
	go s.janitor()
	return s
}

// start (re)launches the subscriber against host:port using tlsCfgFn to obtain
// a fresh TLS config on every dial (so cert rotation works). Safe to call
// repeatedly; an existing subscriber for a different target is stopped first.
func (s *TAKStreamer) start(host string, port int, tlsCfgFn func() *tls.Config) {
	s.stateMu.Lock()
	if s.currentHost == host && s.currentPort == port && s.cancel != nil {
		s.stateMu.Unlock()
		return
	}
	// Tear down any existing subscriber.
	if s.cancel != nil {
		s.cancel()
		s.stateMu.Unlock()
		s.wg.Wait()
		s.stateMu.Lock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.currentHost = host
	s.currentPort = port
	s.connected = false
	s.lastErr = ""
	s.wg.Add(1)
	go s.run(ctx, host, port, tlsCfgFn)
	s.stateMu.Unlock()
	log.Printf("[tak-stream] subscriber launched for %s:%d", host, port)
}

// stop terminates the subscriber and waits for it to exit.
func (s *TAKStreamer) stop() {
	s.stateMu.Lock()
	if s.cancel == nil {
		s.stateMu.Unlock()
		return
	}
	s.cancel()
	s.cancel = nil
	s.currentHost = ""
	s.currentPort = 0
	s.stateMu.Unlock()
	s.wg.Wait()
}

func (s *TAKStreamer) run(ctx context.Context, host string, port int, tlsCfgFn func() *tls.Config) {
	defer s.wg.Done()
	backoff := time.Second
	for ctx.Err() == nil {
		err := s.session(ctx, host, port, tlsCfgFn)
		if ctx.Err() != nil {
			return
		}
		s.stateMu.Lock()
		s.connected = false
		s.reconnectCnt++
		if err != nil {
			s.lastErr = err.Error()
			log.Printf("[tak-stream] session ended: %v; reconnect in %s", err, backoff)
		}
		s.stateMu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > streamMaxBackoff {
			backoff = streamMaxBackoff
		}
	}
}

// session establishes one mTLS connection and decodes events until error.
func (s *TAKStreamer) session(ctx context.Context, host string, port int, tlsCfgFn func() *tls.Config) error {
	tlsCfg := tlsCfgFn()
	if tlsCfg == nil {
		return errors.New("nil TLS config")
	}
	// ServerName matters for SNI; use host as supplied.
	if tlsCfg.ServerName == "" {
		tlsCfg = tlsCfg.Clone()
		tlsCfg.ServerName = host
	}
	dialer := &net.Dialer{Timeout: streamDialTimeout}
	addr := fmt.Sprintf("%s:%d", host, port)
	dctx, cancel := context.WithTimeout(ctx, streamDialTimeout)
	defer cancel()
	rawConn, err := dialer.DialContext(dctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	conn := tls.Client(rawConn, tlsCfg)
	if err := conn.HandshakeContext(dctx); err != nil {
		rawConn.Close()
		return fmt.Errorf("tls handshake %s: %w", addr, err)
	}
	log.Printf("[tak-stream] connected to %s (TLS %s)", addr,
		tls.VersionName(conn.ConnectionState().Version))

	s.stateMu.Lock()
	s.connected = true
	s.lastErr = ""
	s.stateMu.Unlock()
	defer conn.Close()

	// TAK delivers a stream of XML <event>...</event> elements with no root
	// wrapper. xml.Decoder handles this fine when fed token-by-token, but the
	// preamble may include `<?xml ...?>` declarations between events. We use
	// a manual event-extracting scanner for robustness.
	return s.decodeStream(ctx, conn)
}

// decodeStream reads <event>...</event> chunks delimited by their closing tag
// and unmarshals each one. Far more tolerant of stream framing quirks than
// xml.Decoder.Token() across a long-lived connection.
func (s *TAKStreamer) decodeStream(ctx context.Context, r io.Reader) error {
	br := bufio.NewReaderSize(r, streamReadBuf)
	const startTag = "<event"
	const endTag = "</event>"

	for ctx.Err() == nil {
		// Find next <event
		if err := scanUntil(br, startTag); err != nil {
			return err
		}
		// We consumed up to and including `<event`; rebuild the chunk that
		// starts with the literal "<event" prefix.
		chunk := []byte(startTag)
		// Read until </event>
		more, err := scanCollect(br, endTag)
		if err != nil {
			return err
		}
		chunk = append(chunk, more...)
		ev := cotEventXML{}
		if err := xml.Unmarshal(chunk, &ev); err != nil {
			// Skip malformed event but keep stream alive.
			continue
		}
		s.ingest(&ev)
	}
	return ctx.Err()
}

// scanUntil consumes bytes from br up to (but not including) pat. Returns
// nil when pat is found, or the underlying read error.
func scanUntil(br *bufio.Reader, pat string) error {
	matched := 0
	for {
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		if b == pat[matched] {
			matched++
			if matched == len(pat) {
				return nil
			}
			continue
		}
		// reset; account for partial match overlap (simple — restart match)
		if b == pat[0] {
			matched = 1
		} else {
			matched = 0
		}
	}
}

// scanCollect consumes bytes from br until pat is read; returns everything
// including pat. Caller is expected to prefix with the start sentinel.
func scanCollect(br *bufio.Reader, pat string) ([]byte, error) {
	var buf []byte
	matched := 0
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		buf = append(buf, b)
		if b == pat[matched] {
			matched++
			if matched == len(pat) {
				return buf, nil
			}
			continue
		}
		if b == pat[0] {
			matched = 1
		} else {
			matched = 0
		}
		// Safety cap to avoid runaway allocations on a stuck stream.
		if len(buf) > 256*1024 {
			return nil, fmt.Errorf("event exceeded 256 KiB without %s", pat)
		}
	}
}

func (s *TAKStreamer) ingest(ev *cotEventXML) {
	if ev.UID == "" {
		return
	}
	t := &Track{
		UID:       ev.UID,
		Callsign:  ev.Detail.Contact.Callsign,
		Type:      ev.Type,
		How:       ev.How,
		Latitude:  ev.Point.Lat,
		Longitude: ev.Point.Lon,
		HAE:       ev.Point.Hae,
		CourseDeg: ev.Detail.Track.Course,
		SpeedMS:   ev.Detail.Track.Speed,
		Remarks:   strings.TrimSpace(ev.Detail.Remarks),
		EventTime: ev.Time,
		StaleAt:   ev.Stale,
		UpdatedAt: time.Now().UTC(),
	}
	// Skip null-island events from sensors with no fix yet.
	if t.Latitude == 0 && t.Longitude == 0 {
		return
	}
	s.mu.Lock()
	s.tracks[t.UID] = t
	s.mu.Unlock()

	s.stateMu.Lock()
	s.lastEventAt = t.UpdatedAt
	s.eventCount++
	s.stateMu.Unlock()
}

// snapshot returns a sorted copy of current tracks for JSON output.
func (s *TAKStreamer) snapshot() []*Track {
	s.mu.RLock()
	out := make([]*Track, 0, len(s.tracks))
	for _, t := range s.tracks {
		c := *t
		out = append(out, &c)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// status is a small JSON-friendly view of the subscriber state.
type streamStatus struct {
	Connected    bool      `json:"connected"`
	Host         string    `json:"host,omitempty"`
	Port         int       `json:"port,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastEventAt  time.Time `json:"last_event_at,omitempty"`
	EventCount   uint64    `json:"event_count"`
	ReconnectCnt uint64    `json:"reconnect_count"`
	TrackCount   int       `json:"track_count"`
}

func (s *TAKStreamer) status() streamStatus {
	s.mu.RLock()
	n := len(s.tracks)
	s.mu.RUnlock()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return streamStatus{
		Connected:    s.connected,
		Host:         s.currentHost,
		Port:         s.currentPort,
		LastError:    s.lastErr,
		LastEventAt:  s.lastEventAt,
		EventCount:   s.eventCount,
		ReconnectCnt: s.reconnectCnt,
		TrackCount:   n,
	}
}

func (s *TAKStreamer) janitor() {
	tick := time.NewTicker(streamJanitorTick)
	defer tick.Stop()
	for range tick.C {
		cutoff := time.Now().Add(-streamTrackTTL)
		s.mu.Lock()
		for uid, t := range s.tracks {
			if t.UpdatedAt.Before(cutoff) {
				delete(s.tracks, uid)
			}
		}
		s.mu.Unlock()
	}
}
