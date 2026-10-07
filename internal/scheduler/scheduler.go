package scheduler

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"iptvweb/internal/m3u"
)

type Scheduler struct {
	cron     *cron.Cron
	store    *m3u.Store
	m3uURL   string
	interval time.Duration

	mu      sync.Mutex
	running bool
}

func New(store *m3u.Store, m3uURL string, interval time.Duration) *Scheduler {
	return &Scheduler{
		cron:     cron.New(),
		store:    store,
		m3uURL:   m3uURL,
		interval: interval,
	}
}

func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m3uURL == "" || s.interval <= 0 || s.running {
		return nil
	}
	if _, err := s.cron.AddFunc("@every "+s.interval.String(), func() {
		log.Printf("[scheduler] automatic update triggered")
		if err := m3u.FetchAndStore(s.store, s.m3uURL); err != nil {
			log.Printf("[scheduler] automatic update failed error=%v", err)
		}
	}); err != nil {
		return fmt.Errorf("register auto-update job: %w", err)
	}
	s.cron.Start()
	s.running = true
	log.Printf("[scheduler] auto-update enabled interval=%s", s.interval)
	return nil
}

func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.cron.Stop()
		s.running = false
	}
}

func (s *Scheduler) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Scheduler) Trigger() error {
	return m3u.FetchAndStore(s.store, s.m3uURL)
}
