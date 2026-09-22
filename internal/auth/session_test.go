package auth

import (
	"testing"
	"time"
)

func TestSessionStoreExpiry(t *testing.T) {
	clock := newFakeClock()
	s := newSessionStore()
	ttl := 10 * time.Minute

	id, err := s.create(clock.Now(), ttl)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	clock.Advance(ttl - time.Second)
	if _, ok := s.touch(clock.Now(), id, ttl); !ok {
		t.Fatal("session should still be valid just before expiry")
	}

	// touch above slid the deadline forward by ttl again, so advancing past
	// the *original* deadline should NOT expire it - this is exercised in
	// TestSessionStoreSlidingRefresh. Here, create a second session and let
	// it expire untouched.
	id2, err := s.create(clock.Now(), ttl)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clock.Advance(ttl + time.Second)
	if _, ok := s.touch(clock.Now(), id2, ttl); ok {
		t.Fatal("session should have expired")
	}
	if _, ok := s.touch(clock.Now(), id2, ttl); ok {
		t.Fatal("expired session should have been removed from the store")
	}
}

func TestSessionStoreSlidingRefresh(t *testing.T) {
	clock := newFakeClock()
	s := newSessionStore()
	ttl := 10 * time.Minute

	id, err := s.create(clock.Now(), ttl)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Keep "using" the session at intervals shorter than the TTL. It should
	// stay alive well past the original deadline because each touch slides
	// the expiry forward.
	for i := 0; i < 5; i++ {
		clock.Advance(ttl - time.Minute)
		if _, ok := s.touch(clock.Now(), id, ttl); !ok {
			t.Fatalf("iteration %d: session should still be alive via sliding refresh", i)
		}
	}
	// Total elapsed time now well exceeds the original ttl.
}

func TestSessionStoreGC(t *testing.T) {
	clock := newFakeClock()
	s := newSessionStore()
	ttl := time.Minute

	id, err := s.create(clock.Now(), ttl)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clock.Advance(2 * time.Minute)
	s.gc(clock.Now())
	if s.count() != 0 {
		t.Fatalf("expected gc to remove expired session, count=%d", s.count())
	}
	_ = id
}

func TestSessionStoreEvictsOldestWhenFull(t *testing.T) {
	clock := newFakeClock()
	s := newSessionStore()
	ttl := time.Hour

	var first string
	for i := 0; i < maxSessions; i++ {
		id, err := s.create(clock.Now(), ttl)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if i == 0 {
			first = id
		}
		clock.Advance(time.Second)
	}
	if s.count() != maxSessions {
		t.Fatalf("expected %d sessions, got %d", maxSessions, s.count())
	}

	// One more login should evict the oldest (first) session rather than
	// growing the map further.
	if _, err := s.create(clock.Now(), ttl); err != nil {
		t.Fatalf("create overflow: %v", err)
	}
	if s.count() != maxSessions {
		t.Fatalf("expected map to stay capped at %d, got %d", maxSessions, s.count())
	}
	if _, ok := s.touch(clock.Now(), first, ttl); ok {
		t.Fatal("expected the oldest session to have been evicted")
	}
}
