package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"iptvweb/internal/config"
	"iptvweb/internal/m3u"
	webassets "iptvweb/web"
)

type Server struct {
	addr       string
	secret     string
	store      *m3u.Store
	sched      Scheduler
	detectMode string

	hostMu       sync.RWMutex
	allowedHosts map[string]map[string]struct{}

	kindMu sync.RWMutex
	kinds  map[string]string
}

type Scheduler interface {
	Trigger() error
}

const probeTimeout = 5 * time.Second

type StatusResponse struct {
	LastUpdate   string `json:"last_update"`
	ChannelCount int    `json:"channel_count"`
	AutoUpdate   bool   `json:"auto_update"`
	Hash         string `json:"hash"`
}

func New(addr, secret string, store *m3u.Store, sched Scheduler, detectMode string) *Server {
	return &Server{
		addr:         addr,
		secret:       secret,
		store:        store,
		sched:        sched,
		detectMode:   detectMode,
		allowedHosts: make(map[string]map[string]struct{}),
		kinds:        make(map[string]string),
	}
}

func (s *Server) Start() error {
	return s.StartContext(context.Background())
}

func (s *Server) StartContext(ctx context.Context) error {
	httpServer := s.httpServer()
	if ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(shutdownCtx)
		}()
	}

	fmt.Printf("[server] listening on %s\n", s.addr)
	err := httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Addr:              s.addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/channels", s.handleChannels)
	mux.HandleFunc("/api/update", s.handleUpdate)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/stream/", s.handleStream)

	cssFS, err := fs.Sub(webassets.Files, "css")
	if err != nil {
		panic(err)
	}
	jsFS, err := fs.Sub(webassets.Files, "js")
	if err != nil {
		panic(err)
	}
	mux.Handle("/css/", http.StripPrefix("/css/", cachedStaticHandler(cssFS)))
	mux.Handle("/js/", http.StripPrefix("/js/", cachedStaticHandler(jsFS)))

	return securityMiddleware(corsMiddleware(mux))
}

func cachedStaticHandler(files fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webassets.Files.ReadFile("index.html")
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	channels := s.store.Channels()
	s.pruneStreamKinds(channels)
	for i := range channels {
		channels[i].PlaylistURL = s.buildPlaylistURL(channels[i].ID)
		channels[i].StreamURL = s.buildStreamURL(channels[i].ID)
		channels[i].TypeURL = s.buildTypeURL(channels[i].ID)
		channels[i].StreamType = s.channelStreamType(&channels[i])
	}
	writeJSON(w, http.StatusOK, channels)
}

func (s *Server) pruneStreamKinds(channels []m3u.Channel) {
	if len(s.kinds) == 0 {
		return
	}
	live := make(map[string]struct{}, len(channels))
	for i := range channels {
		live[channels[i].ID] = struct{}{}
	}

	s.kindMu.Lock()
	defer s.kindMu.Unlock()
	for id := range s.kinds {
		if _, ok := live[id]; !ok {
			delete(s.kinds, id)
		}
	}
}

func (s *Server) channelStreamType(channel *m3u.Channel) string {
	if s.detectMode != config.DetectModeProbe {
		return m3u.StreamTypeForURL(channel.URL)
	}

	s.kindMu.RLock()
	cached, ok := s.kinds[channel.ID]
	s.kindMu.RUnlock()
	if ok {
		return cached
	}
	return m3u.StreamKindAuto
}

type StreamTypeResponse struct {
	StreamType string `json:"stream_type"`
	StreamURL  string `json:"stream_url"`
}

func (s *Server) handleStreamType(w http.ResponseWriter, r *http.Request, channel *m3u.Channel) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.verifySign(r, s.computeSign(m3u.StreamTypeCanonical(channel.ID))) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, StreamTypeResponse{
		StreamType: s.resolveStreamType(channel),
		StreamURL:  s.buildStreamURL(channel.ID),
	})
}

func (s *Server) resolveStreamType(channel *m3u.Channel) string {
	if s.detectMode != config.DetectModeProbe {
		return m3u.StreamTypeForURL(channel.URL)
	}

	s.kindMu.RLock()
	cached, ok := s.kinds[channel.ID]
	s.kindMu.RUnlock()
	if ok {
		return cached
	}

	kind := s.probeStreamType(channel)
	s.kindMu.Lock()
	s.kinds[channel.ID] = kind
	s.kindMu.Unlock()
	return kind
}

func (s *Server) probeStreamType(channel *m3u.Channel) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	response, err := m3u.FetchResponseWithHeaders(ctx, channel.URL, func(next *url.URL) error {
		return s.validateUpstream(channel, next)
	}, http.Header{"Range": []string{"bytes=0-0"}})
	if err != nil {
		log.Printf("[stream] probe failed channel=%s error=%v", channel.ID, err)
		return m3u.StreamTypeForURL(channel.URL)
	}
	defer response.Body.Close()

	kind := m3u.DetectStreamKind(response.Header.Get("Content-Type"), channel.URL)
	log.Printf("[stream] probed channel=%s kind=%s content_type=%q", channel.ID, kind, response.Header.Get("Content-Type"))
	return kind
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.sched == nil {
		http.Error(w, "scheduler not configured", http.StatusInternalServerError)
		return
	}
	started := time.Now()
	log.Printf("[update] manual update requested")
	if err := s.sched.Trigger(); err != nil {
		log.Printf("[update] manual update failed duration=%s error=%v", time.Since(started).Round(time.Millisecond), err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	_, count, hash := s.store.Status()
	log.Printf("[update] manual update completed duration=%s channels=%d hash=%s", time.Since(started).Round(time.Millisecond), count, hashPreview(hash))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	last, count, hash := s.store.Status()
	autoUpdate := s.sched != nil
	if status, ok := s.sched.(interface{ Enabled() bool }); ok {
		autoUpdate = status.Enabled()
	}
	writeJSON(w, http.StatusOK, StatusResponse{
		LastUpdate:   last.Format(time.RFC3339),
		ChannelCount: count,
		AutoUpdate:   autoUpdate,
		Hash:         hash,
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func hashPreview(hash string) string {
	if hash == "" {
		return "-"
	}
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

func (s *Server) computeSign(canonical string) string {
	if s.secret == "" {
		return ""
	}
	return m3u.ComputeSign(canonical, s.secret)
}

func (s *Server) buildPlaylistURL(channelID string) string {
	playlistPath := m3u.PlaylistCanonical(channelID)
	if s.secret == "" {
		return playlistPath
	}
	return playlistPath + "?sign=" + s.computeSign(playlistPath)
}

func (s *Server) buildStreamURL(channelID string) string {
	streamPath := "/api/stream/" + channelID + "/stream"
	if s.secret == "" {
		return streamPath
	}
	return streamPath + "?sign=" + s.computeSign(m3u.StreamCanonical(channelID))
}

func (s *Server) buildTypeURL(channelID string) string {
	typePath := m3u.StreamTypeCanonical(channelID)
	if s.secret == "" {
		return typePath
	}
	return typePath + "?sign=" + s.computeSign(typePath)
}
func (s *Server) verifySign(r *http.Request, expected string) bool {
	if s.secret == "" {
		return true
	}
	got := r.URL.Query().Get("sign")
	if got == "" || len(got) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pathStr := strings.TrimPrefix(r.URL.Path, "/api/stream/")
	parts := strings.Split(pathStr, "/")
	if len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	channelID, resource := parts[0], parts[1]
	channel := s.store.GetByID(channelID)
	if channel == nil {
		http.NotFound(w, r)
		return
	}

	switch resource {
	case "playlist.m3u8":
		s.handlePlaylist(w, r, channel)
	case "segment":
		s.handleSegment(w, r, channel)
	case "type":
		s.handleStreamType(w, r, channel)
	case "stream":
		s.handleStreamSource(w, r, channel)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleStreamSource(w http.ResponseWriter, r *http.Request, channel *m3u.Channel) {
	if !s.verifySign(r, s.computeSign(m3u.StreamCanonical(channel.ID))) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}
	if kind := s.resolveStreamType(channel); kind == m3u.StreamKindHLS {
		log.Printf("[stream] raw stream rejected channel=%s kind=%s", channel.ID, kind)
		http.Error(w, "upstream source is an HLS playlist", http.StatusUnsupportedMediaType)
		return
	}
	response, err := m3u.FetchResponseWithHeaders(r.Context(), channel.URL, func(next *url.URL) error {
		return s.validateUpstream(channel, next)
	}, http.Header{"Range": r.Header.Values("Range")})
	if err != nil {
		log.Printf("[stream] source failed channel=%s error=%v", channel.ID, err)
		http.Error(w, "fetch stream failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "video/mp2t"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
	for _, header := range []string{"Content-Range", "Accept-Ranges", "Content-Length"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil && r.Context().Err() == nil {
		log.Printf("[stream] source write failed channel=%s error=%v", channel.ID, err)
	}
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request, channel *m3u.Channel) {
	if !s.verifySign(r, s.computeSign(m3u.PlaylistCanonical(channel.ID))) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}

	if kind := s.resolveStreamType(channel); kind != m3u.StreamKindHLS {
		log.Printf("[stream] playlist rejected channel=%s kind=%s", channel.ID, kind)
		http.Error(w, "upstream source is not an HLS playlist", http.StatusUnsupportedMediaType)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	response, err := m3u.FetchResponse(ctx, channel.URL, func(next *url.URL) error {
		return s.validateUpstream(channel, next)
	})
	if err != nil {
		log.Printf("[stream] playlist failed channel=%s error=%v", channel.ID, err)
		http.Error(w, "fetch playlist failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	data, err := m3u.ReadLimited(response.Body, 16<<20)
	if err != nil {
		http.Error(w, "playlist is too large", http.StatusBadGateway)
		return
	}
	base, err := url.Parse(channel.URL)
	if err != nil {
		http.Error(w, "invalid channel source", http.StatusBadGateway)
		return
	}
	parser := m3u.NewPlaylistParser(data, channel.ID, base, s.secret)
	s.rememberHosts(channel.ID, parser.AllowedHosts())
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(parser.Rewrite())
}

func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request, channel *m3u.Channel) {
	segmentURL := r.URL.Query().Get("url")
	if segmentURL == "" {
		http.Error(w, "missing segment URL", http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(segmentURL)
	if err != nil || s.validateUpstream(channel, parsed) != nil {
		http.Error(w, "upstream URL is not allowed", http.StatusForbidden)
		return
	}
	if !s.verifySign(r, s.computeSign(m3u.SegmentCanonical(channel.ID, segmentURL))) {
		http.Error(w, "unauthorized", http.StatusForbidden)
		return
	}

	started := time.Now()
	response, err := m3u.FetchResponseWithHeaders(r.Context(), segmentURL, func(next *url.URL) error {
		return s.validateUpstream(channel, next)
	}, http.Header{"Range": r.Header.Values("Range")})
	if err != nil {
		log.Printf("[stream] segment failed channel=%s target=%s duration=%s error=%v", channel.ID, upstreamSummary(segmentURL), time.Since(started).Round(time.Millisecond), err)
		http.Error(w, "fetch stream resource failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	contentType := response.Header.Get("Content-Type")
	normalized := strings.ToLower(contentType)
	if idx := strings.IndexByte(normalized, ';'); idx >= 0 {
		normalized = strings.TrimSpace(normalized[:idx])
	}
	isPlaylist := strings.Contains(normalized, "mpegurl") || strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8")
	if isPlaylist {
		data, readErr := m3u.ReadLimited(response.Body, 16<<20)
		if readErr != nil {
			http.Error(w, "nested playlist is too large", http.StatusBadGateway)
			return
		}
		parser := m3u.NewPlaylistParser(data, channel.ID, parsed, s.secret)
		s.rememberHosts(channel.ID, parser.AllowedHosts())
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(parser.Rewrite())
		return
	}

	w.Header().Set("Content-Type", m3u.SegmentContentType(contentType))
	for _, header := range []string{"Content-Range", "Accept-Ranges", "Content-Length"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil && r.Context().Err() == nil {
		log.Printf("[stream] segment write failed channel=%s target=%s duration=%s error=%v", channel.ID, upstreamSummary(segmentURL), time.Since(started).Round(time.Millisecond), err)
	}
}

func upstreamSummary(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "invalid-url"
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.Path
}

func (s *Server) validateUpstream(channel *m3u.Channel, target *url.URL) error {
	if target == nil || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return fmt.Errorf("invalid upstream URL")
	}
	source, err := url.Parse(channel.URL)
	if err != nil {
		return fmt.Errorf("invalid channel source")
	}
	host := strings.ToLower(target.Host)
	if host == strings.ToLower(source.Host) {
		return nil
	}
	if !s.isAllowedHost(channel.ID, host) || !m3u.IsSafeProxyURL(target) {
		return fmt.Errorf("upstream host is not allowed")
	}
	return nil
}

func (s *Server) rememberHosts(channelID string, hosts map[string]struct{}) {
	s.hostMu.Lock()
	defer s.hostMu.Unlock()
	allowed := s.allowedHosts[channelID]
	if allowed == nil {
		allowed = make(map[string]struct{})
		s.allowedHosts[channelID] = allowed
	}
	for host := range hosts {
		allowed[strings.ToLower(host)] = struct{}{}
	}
}

func (s *Server) isAllowedHost(channelID, host string) bool {
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	_, ok := s.allowedHosts[channelID][strings.ToLower(host)]
	return ok
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; worker-src 'self' blob:; style-src 'self'; media-src 'self' blob:; img-src 'self' data: https:; connect-src 'self'; object-src 'none'; base-uri 'self'")
		next.ServeHTTP(w, r)
	})
}
