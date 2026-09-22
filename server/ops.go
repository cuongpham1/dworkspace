package server

import (
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Operational hardening (Welle 14): login brute-force throttling, a background
// cleanup loop (expired sessions / idempotency keys / stale rate buckets /
// trash auto-purge), readiness health, and per-field input caps.

const (
	maxTitleLen   = 2000
	maxCommentLen = 10000
)

// clientIP extracts the caller's IP. Proxy headers (X-Forwarded-For /
// X-Real-Ip) are only honored when the admin enabled "trust_proxy" (running
// behind Caddy/nginx/Cloudflare) — without a proxy those headers are
// client-controlled and would let an attacker rotate fake IPs past the login
// rate limit.
func (s *Server) clientIP(r *http.Request) string {
	if s.boolSetting("trust_proxy") {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if rip := r.Header.Get("X-Real-Ip"); rip != "" {
			return strings.TrimSpace(rip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// evict drops rate-limit buckets that are full (idle), keeping the map from
// growing without bound over the process lifetime.
func (rl *rateLimiter) evict() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for k, b := range rl.buckets {
		// Refill to current, then drop any bucket back at full capacity.
		elapsed := now.Sub(b.last).Seconds()
		if b.tokens+elapsed*rl.rate >= rl.burst {
			delete(rl.buckets, k)
		}
	}
}

// trashRetentionDays returns how long trashed pages are kept before automatic
// permanent deletion. Admin setting > DWORKSPACE_TRASH_DAYS env > 30-day default;
// 0 disables auto-purge.
func (s *Server) trashRetentionDays() int {
	if v := s.setting("trash_days", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	if v := Env("TRASH_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 30
}

// PurgeTrashedBefore permanently deletes trashed pages older than cutoff and
// returns how many went, INCLUDING the search index entries that hang off them.
//
// The index part is the reason this is a function and not the one-line DELETE
// it used to be. pages_fts and chunks_fts are virtual tables: no foreign keys,
// no cascade, nothing that notices a page leaving. So every automatic purge
// since the retention setting existed has been dropping rows out of pages while
// leaving their words in the index — invisible, because the search JOINs
// against pages and quietly discards what it cannot resolve.
//
// It stopped being invisible when chunks_fts became an external-content table.
// Its rowid is page_chunks.seq, an INTEGER PRIMARY KEY, and SQLite hands the
// next insert the number the deleted row gave up. An orphaned index entry then
// describes a passage that belongs to somebody else's page, and a search for a
// word from a document purged last month opens a document that never contained
// it. See chunks_index_test.go.
//
// The subtree matters too: the DELETE cascades through parent_id, so pages the
// cutoff itself never selected can still disappear. The index has to be told
// about those as well, hence the recursive term.
func (s *Server) PurgeTrashedBefore(cutoff string) (int, error) {
	const doomed = `WITH RECURSIVE doomed(id) AS (
			SELECT id FROM pages WHERE trashed_at IS NOT NULL AND trashed_at < ?
			UNION
			SELECT p.id FROM pages p JOIN doomed ON p.parent_id = doomed.id
		) SELECT id FROM doomed`

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Before the pages go — the index reads page_chunks to forget a row.
	if err := deleteChunkIndex(tx, `page_id IN (`+doomed+`)`, cutoff); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM pages_fts WHERE id IN (`+doomed+`)`, cutoff); err != nil {
		return 0, err
	}
	// Extracted file text predates its foreign key on some instances, so
	// CASCADE cannot be relied on for it (same reason as handleDeletePage).
	if _, err := tx.Exec(`DELETE FROM file_texts WHERE page_id IN (`+doomed+`)`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM pages WHERE trashed_at IS NOT NULL AND trashed_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// startCleanup runs periodic maintenance until stopCleanup is closed.
func (s *Server) startCleanup() {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			s.runCleanup()
			select {
			case <-s.stopCleanup:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) runCleanup() {
	s.deleteExpiredSessions()
	// Idempotency keys are only useful for a short retry window.
	s.db.Exec(`DELETE FROM idempotency WHERE created_at < ?`, time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339Nano))
	s.mcpRate.evict()
	s.loginRate.evict()
	s.tokenRate.evict()
	s.sweepOAuth()
	// Once a day at most, and it returns immediately on the other 47 ticks.
	// Inline rather than in its own goroutine so it cannot outlive shutdown.
	s.checkForUpdate()
	if days := s.trashRetentionDays(); days > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
		if n, err := s.PurgeTrashedBefore(cutoff); err != nil {
			log.Printf("trash purge: %v", err)
		} else if n > 0 {
			log.Printf("trash: purged %d pages older than %d days", n, days)
		}
	}
	// The activity log is the only table here that nothing ever bounded. It grows
	// with every change forever, which is right by default (see
	// auditRetentionDays) and wrong for an instance that has to promise its
	// people a limit. Off unless an admin sets a period.
	s.pruneAuditLog()
}

// handleHealth is a readiness probe: it pings the DB so orchestration can tell a
// live-but-broken instance from a healthy one.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"unavailable"}`))
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "version": Version})
}

// Env reads an environment variable under its DWORKSPACE_ name.
func Env(name string) string {
	return os.Getenv("DWORKSPACE_" + name)
}

// EnvOr is Env with a default.
func EnvOr(name, fallback string) string {
	if v := Env(name); v != "" {
		return v
	}
	return fallback
}
