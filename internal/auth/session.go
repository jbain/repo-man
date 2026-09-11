package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// maxSessions bounds the number of concurrently live sessions. A
// single-operator dashboard has no legitimate reason to need more than a
// handful at once; the cap exists so a forgotten script, a browser that
// never sends cookies back, or a leaked passphrase being replayed can't grow
// the session map without bound. When full, the session with the oldest
// creation time is evicted to make room for the new login.
const maxSessions = 64

// sessionRecord is the server-side state for one session. The map key it's
// stored under (see sessionStore) is the only thing the cookie carries —
// the ID itself must be unguessable, since possessing it is equivalent to
// being logged in.
type sessionRecord struct {
	created      time.Time
	expires      time.Time // sliding deadline; pushed forward by touch
	cookieIssued time.Time // when the browser cookie was last (re)written
}

// sessionStore holds live sessions in memory only. See the package doc for
// why there is no on-disk persistence.
type sessionStore struct {
	mu   sync.RWMutex
	byID map[string]*sessionRecord
}

func newSessionStore() *sessionStore {
	return &sessionStore{byID: make(map[string]*sessionRecord)}
}

// create starts a new session with the given sliding TTL and returns its ID.
func (s *sessionStore) create(now time.Time, ttl time.Duration) (string, error) {
	id, err := newSessionID()
	if err != nil {
		return "", err
	}
	rec := &sessionRecord{created: now, expires: now.Add(ttl), cookieIssued: now}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.byID) >= maxSessions {
		s.evictOldestLocked()
	}
	s.byID[id] = rec
	return id, nil
}

// evictOldestLocked removes the session with the earliest creation time.
// Callers must hold mu for writing.
func (s *sessionStore) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for id, rec := range s.byID {
		if oldestID == "" || rec.created.Before(oldest) {
			oldestID, oldest = id, rec.created
		}
	}
	if oldestID != "" {
		delete(s.byID, oldestID)
	}
}

// touch validates id and, if it names a live session, slides its expiry
// forward by ttl. It returns when the session's cookie was last issued (so
// the caller can decide whether to re-send the cookie) and whether the
// session is valid. An expired session is deleted as a side effect.
func (s *sessionStore) touch(now time.Time, id string, ttl time.Duration) (cookieIssued time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, found := s.byID[id]
	if !found {
		return time.Time{}, false
	}
	if now.After(rec.expires) {
		delete(s.byID, id)
		return time.Time{}, false
	}
	rec.expires = now.Add(ttl)
	return rec.cookieIssued, true
}

// markCookieIssued records that the session's cookie was just rewritten, so
// the next call to touch reports an up-to-date cookieIssued.
func (s *sessionStore) markCookieIssued(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.byID[id]; ok {
		rec.cookieIssued = now
	}
}

// delete removes a session unconditionally, used by Logout.
func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// gc drops sessions past their deadline. touch already does this lazily for
// sessions that get used again, but an abandoned session (browser closed,
// cookie never comes back) would otherwise sit in the map until process
// exit; Run calls gc periodically to bound that.
func (s *sessionStore) gc(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rec := range s.byID {
		if now.After(rec.expires) {
			delete(s.byID, id)
		}
	}
}

// count reports the number of live sessions. Used by tests.
func (s *sessionStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// newSessionID returns a 32-byte cryptographically random session ID,
// base64url-encoded (no padding) so it's directly usable as a cookie value
// with no characters that need quoting.
func newSessionID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
