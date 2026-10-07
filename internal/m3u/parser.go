package m3u

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

type Channel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TVGID       string `json:"tvg_id,omitempty"`
	Logo        string `json:"logo"`
	Group       string `json:"group"`
	URL         string `json:"-"`
	PlaylistURL string `json:"playlist_url"`
	StreamURL   string `json:"stream_url,omitempty"`
	TypeURL     string `json:"type_url,omitempty"`
	StreamType  string `json:"stream_type,omitempty"`
}

const (
	StreamKindHLS  = "hls"
	StreamKindTS   = "ts"
	StreamKindAuto = "auto"

	maxPlaylistSize        = 16 << 20
	maxSegmentSize         = 32 << 20
	maxRedirects           = 5
	upstreamRequestTimeout = 15 * time.Second
)

func DetectStreamKind(contentType, rawURL string) string {
	if IsHLSContentType(contentType) {
		return StreamKindHLS
	}
	return StreamTypeForURL(rawURL)
}

func IsHLSContentType(contentType string) bool {
	normalized := strings.ToLower(strings.TrimSpace(contentType))
	if normalized == "" {
		return false
	}
	if idx := strings.IndexByte(normalized, ';'); idx >= 0 {
		normalized = strings.TrimSpace(normalized[:idx])
	}
	return strings.Contains(normalized, "mpegurl")
}

var (
	attrRe     = regexp.MustCompile(`(?i)([A-Za-z0-9_-]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s,]+))`)
	idStripRe  = regexp.MustCompile(`[^a-z0-9]+`)
	uriAttrRe  = regexp.MustCompile(`(?i)(\bURI\s*=\s*")([^"]+)(")`)
	sharedHTTP = &http.Client{Timeout: 0, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConns: 32, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, ExpectContinueTimeout: 1 * time.Second}}
)

type RedirectValidator func(*url.URL) error

func parseExtinf(line string) (name, tvgID, logo, group string) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "#EXTINF:"))
	attributeText := rest
	if first := strings.IndexByte(rest, ','); first >= 0 {
		attributeText = strings.TrimSpace(rest[:first])
		name = strings.TrimSpace(rest[first+1:])
		if last := strings.LastIndexByte(rest, ','); last > first {
			candidate := strings.TrimSpace(rest[first+1 : last])
			if len(attrRe.FindAllStringSubmatch(candidate, -1)) > 0 {
				attributeText = candidate
				name = strings.TrimSpace(rest[last+1:])
			}
		}
	}
	for _, m := range attrRe.FindAllStringSubmatch(attributeText, -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		if v == "" {
			v = m[4]
		}
		switch strings.ToLower(m[1]) {
		case "tvg-id":
			tvgID = v
		case "tvg-name":
			if name == "" {
				name = v
			}
		case "tvg-logo":
			logo = v
		case "group-title":
			group = v
		}
	}
	return
}
func Parse(data []byte) ([]Channel, error) {
	channels := make([]Channel, 0)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var current Channel
	used := make(map[string]int)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\xEF\xBB\xBF"))
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			current.Name, current.TVGID, current.Logo, current.Group = parseExtinf(line)
		case strings.HasPrefix(line, "#"):
		default:
			current.URL = line
			if current.Name == "" {
				current.Name = displayName(line)
			}
			current.ID = uniqueID(generateID(current.Name, line), line, used)
			channels = append(channels, current)
			current = Channel{}
		}
	}
	return channels, scanner.Err()
}
func displayName(raw string) string {
	p, e := url.Parse(raw)
	if e == nil && p.Path != "" {
		if n := path.Base(p.Path); n != "." && n != "/" && n != "" {
			return n
		}
	}
	return raw
}
func generateID(name, u string) string {
	slug := strings.Trim(idStripRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-")
	if slug == "" {
		slug = "channel"
	}
	h := md5.Sum([]byte(u))
	return slug + "-" + hex.EncodeToString(h[:])[:8]
}
func uniqueID(base, u string, used map[string]int) string {
	if used[base] == 0 {
		used[base] = 1
		return base
	}
	used[base]++
	h := md5.Sum([]byte(fmt.Sprintf("%s#%d", u, used[base])))
	c := base + "-" + hex.EncodeToString(h[:])[:6]
	used[c] = 1
	return c
}
func Fetch(u string) ([]byte, error) { return FetchContext(context.Background(), u) }
func FetchContext(ctx context.Context, u string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, upstreamRequestTimeout)
		defer c()
	}
	r, e := FetchResponse(ctx, u, nil)
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	return readLimited(r.Body, maxPlaylistSize)
}
func FetchResponse(ctx context.Context, u string, v RedirectValidator) (*http.Response, error) {
	return FetchResponseWithHeaders(ctx, u, v, nil)
}
func FetchResponseWithHeaders(ctx context.Context, u string, v RedirectValidator, h http.Header) (*http.Response, error) {
	c := *sharedHTTP
	if v != nil {
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many upstream redirects")
			}
			return v(req.URL)
		}
	}
	q, e := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if e != nil {
		return nil, errors.New("invalid upstream request URL")
	}
	q.Header.Set("User-Agent", "iptvweb/1.0")
	for k, vs := range h {
		for _, x := range vs {
			q.Header.Add(k, x)
		}
	}
	r, e := c.Do(q)
	if e != nil {
		return nil, fmt.Errorf("upstream request failed: %v", e)
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		r.Body.Close()
		return nil, fmt.Errorf("unexpected upstream status %d", r.StatusCode)
	}
	return r, nil
}
func readLimited(r io.Reader, n int64) ([]byte, error) {
	d, e := io.ReadAll(io.LimitReader(r, n+1))
	if e != nil {
		return nil, e
	}
	if int64(len(d)) > n {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", n)
	}
	return d, nil
}
func ReadLimited(r io.Reader, n int64) ([]byte, error) { return readLimited(r, n) }
func IsSafeProxyURL(t *url.URL) bool {
	if t == nil || (t.Scheme != "http" && t.Scheme != "https") || t.Hostname() == "" {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(t.Hostname(), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return isPublicIP(ip)
	}
	ips, e := net.LookupIP(h)
	if e != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return false
		}
	}
	return true
}
func isPublicIP(ip net.IP) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

type PlaylistParser struct {
	channelID string
	baseURL   *url.URL
	lines     []string
	secret    string
}

func NewPlaylistParser(d []byte, id string, b *url.URL, s string) *PlaylistParser {
	return &PlaylistParser{channelID: id, baseURL: b, lines: strings.Split(string(d), "\n"), secret: s}
}
func (p *PlaylistParser) ResolveURL(raw string) string {
	if p.baseURL == nil {
		return raw
	}
	a, e := p.baseURL.Parse(strings.TrimSpace(raw))
	if e != nil {
		return raw
	}
	return a.String()
}
func (p *PlaylistParser) proxyURL(raw string) string {
	u := p.ResolveURL(raw)
	x := url.QueryEscape(u)
	z := "/api/stream/" + p.channelID + "/segment?url=" + x
	if p.secret != "" {
		z += "&sign=" + ComputeSign(SegmentCanonical(p.channelID, u), p.secret)
	}
	return z
}
func (p *PlaylistParser) AllowedHosts() map[string]struct{} {
	h := map[string]struct{}{}
	for _, l := range p.lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#EXTM3U") {
			continue
		}
		if strings.HasPrefix(t, "#") {
			for _, m := range uriAttrRe.FindAllStringSubmatch(t, -1) {
				p.addHost(h, p.ResolveURL(m[2]))
			}
		} else {
			p.addHost(h, p.ResolveURL(t))
		}
	}
	return h
}
func (p *PlaylistParser) addHost(h map[string]struct{}, raw string) {
	u, e := url.Parse(raw)
	if e == nil && u.Host != "" {
		h[strings.ToLower(u.Host)] = struct{}{}
	}
}
func (p *PlaylistParser) Rewrite() []byte {
	out := make([]string, 0, len(p.lines))
	for _, l := range p.lines {
		t := strings.TrimSpace(l)
		if t == "" {
			out = append(out, l)
		} else if strings.HasPrefix(t, "#") {
			out = append(out, uriAttrRe.ReplaceAllStringFunc(l, func(m string) string {
				x := uriAttrRe.FindStringSubmatch(m)
				if len(x) != 4 {
					return m
				}
				return x[1] + p.proxyURL(x[2]) + x[3]
			}))
		} else {
			out = append(out, p.proxyURL(t))
		}
	}
	return []byte(strings.Join(out, "\n"))
}
func PlaylistCanonical(id string) string { return "/api/stream/" + id + "/playlist.m3u8" }
func StreamCanonical(id string) string   { return "/api/stream/" + id + "/stream" }
func StreamTypeForURL(raw string) string {
	p, e := url.Parse(raw)
	if e == nil && strings.HasSuffix(strings.ToLower(p.Path), ".m3u8") {
		return "hls"
	}
	return "ts"
}
func StreamTypeCanonical(id string) string { return "/api/stream/" + id + "/type" }
func SegmentContentType(contentType string) string {
	normalized := strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.IndexByte(normalized, ';'); idx >= 0 {
		normalized = strings.TrimSpace(normalized[:idx])
	}
	switch normalized {
	case "", "application/octet-stream", "binary/octet-stream", "application/force-download", "text/plain":
		return "video/mp2t"
	}
	if contentType == "" {
		return "video/mp2t"
	}
	return contentType
}

func SegmentCanonical(id, u string) string { return "/api/stream/" + id + "/segment?url=" + u }

func ComputeSign(c, s string) string { h := md5.Sum([]byte(c + s)); return hex.EncodeToString(h[:]) }
