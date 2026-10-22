package claim

import (
	"regexp"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestIssueConsumeSingleUse(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	s := NewStore(300*time.Second, c.now)
	tok, err := s.Issue(42, "jit-blob")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(tok) {
		t.Fatalf("token %q is not 32 lowercase hex chars", tok)
	}
	if !s.Pending(42) {
		t.Error("Pending(42) = false after Issue")
	}
	jit, job, ok := s.Consume(tok)
	if !ok || jit != "jit-blob" || job != 42 {
		t.Fatalf("Consume = %q %d %v", jit, job, ok)
	}
	if _, _, ok := s.Consume(tok); ok {
		t.Error("second Consume succeeded; claims must be single use")
	}
	if s.Pending(42) {
		t.Error("Pending(42) = true after Consume")
	}
}

func TestTokensAreUnique(t *testing.T) {
	s := NewStore(time.Minute, nil)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := s.Issue(int64(i), "x")
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token after %d issues", i)
		}
		seen[tok] = true
	}
}

func TestTTL(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	s := NewStore(300*time.Second, c.now)
	a, _ := s.Issue(1, "a")
	b, _ := s.Issue(2, "b")
	c.t = c.t.Add(299 * time.Second)
	if _, _, ok := s.Consume(a); !ok {
		t.Error("claim rejected before its TTL")
	}
	c.t = c.t.Add(time.Second)
	if _, _, ok := s.Consume(b); ok {
		t.Error("claim accepted at its TTL")
	}
	if s.Len() != 0 {
		t.Errorf("expired claim not deleted on read: Len = %d", s.Len())
	}
}

func TestSweepAndDropJob(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	s := NewStore(time.Minute, c.now)
	_, _ = s.Issue(1, "a")
	c.t = c.t.Add(30 * time.Second)
	keep, _ := s.Issue(2, "b")
	drop, _ := s.Issue(3, "c")
	c.t = c.t.Add(45 * time.Second)
	if n := s.Sweep(); n != 1 {
		t.Errorf("Sweep removed %d, want 1", n)
	}
	s.DropJob(3)
	if _, _, ok := s.Consume(drop); ok {
		t.Error("dropped claim still consumable")
	}
	if _, _, ok := s.Consume(keep); !ok {
		t.Error("unrelated claim lost")
	}
}

func TestConsumeRejectsMalformed(t *testing.T) {
	s := NewStore(time.Minute, nil)
	for _, tok := range []string{"", "abc", "zz" + "0123456789abcdef0123456789abcdef"} {
		if _, _, ok := s.Consume(tok); ok {
			t.Errorf("Consume(%q) succeeded", tok)
		}
	}
}
