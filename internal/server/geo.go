package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"orbitbeacon/internal/server/egress"
	"orbitbeacon/internal/server/nodes"
	"orbitbeacon/internal/server/storage"
	"orbitbeacon/internal/timekey"
)

var geoCountryPattern = regexp.MustCompile(`^[A-Za-z]{2}$`)

var geoExcluded = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("::ffff:0:0:0/96"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
}

func geoPublicIP(raw string) bool {
	ip, err := netip.ParseAddr(raw)
	if err != nil || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range geoExcluded {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// One worker owns network pacing. Queue entries remain deduplicated while in flight.
type geoWorker struct {
	nodes          *nodes.Service
	client         *http.Client
	baseURL, token string
	queue          chan string
	mu             sync.Mutex
	pending        map[string]bool
	next           time.Time
	backoff        time.Duration
}

func newGeoWorker(ns *nodes.Service, token string) *geoWorker {
	dial := &egress.Dialer{Resolver: net.DefaultResolver, Dial: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: dial.DialContext}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("geo redirect denied") }}
	return &geoWorker{nodes: ns, client: client, baseURL: "https://ipinfo.io", token: token, queue: make(chan string, 256), pending: map[string]bool{}}
}
func (g *geoWorker) enqueue(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending[id] {
		return
	}
	select {
	case g.queue <- id:
		g.pending[id] = true
	default:
	}
}

// RunGeo shares the server background context and is drained before storage closes.
func (h *handler) RunGeo(ctx context.Context) {
	if h.geo != nil {
		h.geo.run(ctx)
	}
}
func (g *geoWorker) run(ctx context.Context) {
	defer g.client.CloseIdleConnections()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-g.queue:
			g.lookup(ctx, id)
			g.mu.Lock()
			delete(g.pending, id)
			g.mu.Unlock()
		}
	}
}
func (g *geoWorker) lookup(ctx context.Context, id string) {
	// Wait before reading eligibility, so a queued manual region edit wins.
	if wait := time.Until(g.next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
	lease, err := g.nodes.Database().Acquire(ctx, storage.Reader)
	if err != nil {
		return
	}
	var ip, region, checked string
	err = lease.QueryRowContext(lease.Context(), "SELECT remote_ip,region,geo_checked_at FROM nodes WHERE id=?", id).Scan(&ip, &region, &checked)
	lease.Close()
	if err != nil || region != "" || !geoPublicIP(ip) {
		return
	}
	at, err := timekey.Parse(checked)
	if err == nil && time.Since(at) < 24*time.Hour {
		return
	}
	endpoint := g.baseURL + "/" + url.PathEscape(ip) + "/json"
	if g.token != "" {
		endpoint += "?token=" + url.QueryEscape(g.token)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	g.next = time.Now().Add(time.Second)
	response, err := g.client.Do(req)
	if err != nil {
		g.delay()
		return
	}
	defer response.Body.Close()
	if response.StatusCode == 429 || response.StatusCode >= 500 {
		g.delay()
		return
	}
	// Mark unsuccessful/invalid responses checked too: never retry them on every report.
	country, location := "", ""
	if response.StatusCode == 200 {
		body, err := io.ReadAll(io.LimitReader(response.Body, 16385))
		var data struct {
			Country string `json:"country"`
			Region  string `json:"region"`
			City    string `json:"city"`
		}
		if err == nil && len(body) <= 16384 && utf8.Valid(body) && json.Unmarshal(body, &data) == nil && geoCountryPattern.MatchString(data.Country) {
			location = data.Region
			if location == "" {
				location = data.City
			}
			if utf8.ValidString(location) && len(location) <= 255 && !strings.ContainsFunc(location, unicode.IsControl) {
				country = strings.ToUpper(data.Country)
				location = strings.TrimSpace(location)
			} else {
				location = ""
			}
		}
	}
	g.backoff = 0
	writer, err := g.nodes.Database().Acquire(ctx, storage.Writer)
	if err != nil {
		return
	}
	defer writer.Close()
	tx, err := writer.BeginWrite(writer.Context())
	if err != nil {
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(writer.Context(), "UPDATE nodes SET geo_country=?,geo_location=?,geo_checked_at=? WHERE id=? AND remote_ip=? AND region=''", country, location, timekey.Format(time.Now()), id, ip)
	if err == nil {
		if n, _ := result.RowsAffected(); n > 0 && tx.Commit() == nil {
			g.nodes.InvalidatePublic()
		}
	}
}
func (g *geoWorker) delay() {
	if g.backoff == 0 {
		g.backoff = time.Minute
	} else {
		g.backoff *= 2
		if g.backoff > time.Hour {
			g.backoff = time.Hour
		}
	}
	g.next = time.Now().Add(g.backoff)
}
