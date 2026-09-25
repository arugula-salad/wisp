package server

import (
	"os"
	"path/filepath"
	"time"

	"github.com/arugula-salad/wisp/internal/httpstats"
)

// Request metrics for the web UI are counted by internal/httpstats; the server
// keeps them in the data directory across restarts.

func (s *Server) httpStatsFile() string { return filepath.Join(s.opts.DataDir, "http-stats.json") }

// SaveHTTPStats writes the day's request figures out; the daemon calls it on
// the way down, and the sampler every minute.
func (s *Server) SaveHTTPStats() {
	if err := s.httpStats.Save(s.httpStatsFile()); err != nil {
		s.log.Warn("could not save request metrics", "err", err)
	}
}

func (s *Server) runHTTPStats() {
	if err := s.httpStats.Load(s.httpStatsFile(), time.Now()); err != nil && !os.IsNotExist(err) {
		s.log.Warn("could not load saved request metrics; starting over", "err", err)
	}
	t := time.NewTicker(httpstats.SaveEvery)
	defer t.Stop()
	for range t.C {
		s.SaveHTTPStats()
	}
}
