package m3u

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type storeSnapshot struct {
	channels   []Channel
	byID       map[string]Channel
	lastUpdate time.Time
	hash       string
}

type Store struct {
	state   atomic.Value
	updateM sync.Mutex
}

func NewStore() *Store {
	s := &Store{}
	s.state.Store(storeSnapshot{
		channels: []Channel{},
		byID:     make(map[string]Channel),
	})
	return s
}

func (s *Store) snapshot() storeSnapshot {
	return s.state.Load().(storeSnapshot)
}

func (s *Store) Channels() []Channel {
	channels := s.snapshot().channels
	if len(channels) == 0 {
		return []Channel{}
	}
	return append([]Channel(nil), channels...)
}

func (s *Store) ChannelCount() int {
	return len(s.snapshot().channels)
}

func (s *Store) LastUpdate() time.Time {
	return s.snapshot().lastUpdate
}

func (s *Store) Hash() string {
	return s.snapshot().hash
}

func (s *Store) Status() (time.Time, int, string) {
	current := s.snapshot()
	return current.lastUpdate, len(current.channels), current.hash
}

func (s *Store) Update(channels []Channel) {
	s.state.Store(makeSnapshot(channels, time.Now(), ""))
}

func makeSnapshot(channels []Channel, updatedAt time.Time, hash string) storeSnapshot {
	ordered := make([]Channel, len(channels))
	copy(ordered, channels)
	byID := make(map[string]Channel, len(ordered))
	for _, channel := range ordered {
		byID[channel.ID] = channel
	}
	if hash == "" {
		hash = ComputeHash(ordered)
	}
	return storeSnapshot{
		channels:   ordered,
		byID:       byID,
		lastUpdate: updatedAt,
		hash:       ComputeHash(ordered),
	}
}

func (s *Store) GetByID(id string) *Channel {
	channel, ok := s.snapshot().byID[id]
	if !ok {
		return nil
	}
	return &channel
}

func FetchAndStore(s *Store, m3uURL string) error {
	started := time.Now()
	log.Printf("[m3u] update started")

	s.updateM.Lock()
	defer s.updateM.Unlock()

	data, err := Fetch(m3uURL)
	if err != nil {
		log.Printf("[m3u] update failed stage=fetch duration=%s error=%v", time.Since(started).Round(time.Millisecond), err)
		return err
	}
	log.Printf("[m3u] playlist fetched bytes=%d duration=%s", len(data), time.Since(started).Round(time.Millisecond))

	channels, err := Parse(data)
	if err != nil {
		log.Printf("[m3u] update failed stage=parse duration=%s error=%v", time.Since(started).Round(time.Millisecond), err)
		return err
	}
	newHash := ComputeHash(channels)
	current := s.snapshot()
	if newHash == current.hash {
		current.lastUpdate = time.Now()
		s.state.Store(current)
		log.Printf("[m3u] update completed changed=false channels=%d hash=%s duration=%s", len(channels), hashPreview(newHash), time.Since(started).Round(time.Millisecond))
		return nil
	}
	s.state.Store(makeSnapshot(channels, time.Now(), newHash))
	log.Printf("[m3u] update completed changed=true channels=%d hash=%s duration=%s", len(channels), hashPreview(newHash), time.Since(started).Round(time.Millisecond))
	return nil
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

func FetchSegment(upstreamURL string) ([]byte, string, error) {
	response, err := FetchResponse(context.Background(), upstreamURL, nil)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	data, err := ReadLimited(response.Body, maxSegmentSize)
	if err != nil {
		return nil, "", err
	}
	return data, responseContentType(response.Header.Get("Content-Type"), data), nil
}

func responseContentType(header string, data []byte) string {
	if header != "" {
		return header
	}
	contentType := http.DetectContentType(data)
	if strings.Contains(contentType, "application/octet-stream") {
		return "video/mp2t"
	}
	return contentType
}

func ComputeHash(channels []Channel) string {
	if len(channels) == 0 {
		return ""
	}

	ordered := append([]Channel(nil), channels...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID != ordered[j].ID {
			return ordered[i].ID < ordered[j].ID
		}
		if ordered[i].Name != ordered[j].Name {
			return ordered[i].Name < ordered[j].Name
		}
		if ordered[i].TVGID != ordered[j].TVGID {
			return ordered[i].TVGID < ordered[j].TVGID
		}
		if ordered[i].Group != ordered[j].Group {
			return ordered[i].Group < ordered[j].Group
		}
		if ordered[i].Logo != ordered[j].Logo {
			return ordered[i].Logo < ordered[j].Logo
		}
		return ordered[i].URL < ordered[j].URL
	})

	h := md5.New()
	writeField := func(value string) {
		h.Write([]byte(strconv.Itoa(len(value))))
		h.Write([]byte(":"))
		h.Write([]byte(value))
	}
	for _, channel := range ordered {
		writeField(channel.ID)
		writeField(channel.Name)
		writeField(channel.TVGID)
		writeField(channel.Group)
		writeField(channel.Logo)
		writeField(channel.URL)
	}
	return hex.EncodeToString(h.Sum(nil))
}
