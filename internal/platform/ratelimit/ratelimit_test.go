package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var rule = Rule{Name: "r", Limit: 3, Window: time.Minute}

func newClock() (*Memory, *time.Time) {
	m := NewMemory()
	now := time.Now()
	m.now = func() time.Time { return now }
	return m, &now
}

func allow(t *testing.T, m *Memory, key string, r Rule) (bool, time.Duration) {
	t.Helper()
	ok, wait, err := m.Allow(context.Background(), key, r)
	if err != nil {
		t.Fatal(err)
	}
	return ok, wait
}

func TestALimitHoldsAndTheWaitIsTrue(t *testing.T) {
	m, now := newClock()
	for i := 0; i < 3; i++ {
		if ok, _ := allow(t, m, "a", rule); !ok {
			t.Fatalf("request %d was refused", i+1)
		}
	}
	*now = now.Add(20 * time.Second)
	ok, wait := allow(t, m, "a", rule)
	if ok || wait != 40*time.Second {
		t.Errorf("%v, %s", ok, wait)
	}
	*now = now.Add(wait)
	if ok, _ := allow(t, m, "a", rule); !ok {
		t.Error("the next window must allow it")
	}
}

func TestRefusedRequestsDoNotExtendTheWait(t *testing.T) {
	m, now := newClock()
	for i := 0; i < 3; i++ {
		allow(t, m, "a", rule)
	}
	for i := 0; i < 5; i++ {
		*now = now.Add(10 * time.Second)
		allow(t, m, "a", rule)
	}
	*now = now.Add(10 * time.Second)
	if ok, _ := allow(t, m, "a", rule); !ok {
		t.Error("repeating a refused request must not keep someone out for longer")
	}
}

func TestKeysAndRulesAreCountedApart(t *testing.T) {
	m, _ := newClock()
	for i := 0; i < 3; i++ {
		allow(t, m, "a", rule)
	}
	if ok, _ := allow(t, m, "b", rule); !ok {
		t.Error("another key was limited")
	}
	if ok, _ := allow(t, m, "a", Rule{Name: "other", Limit: 3, Window: time.Minute}); !ok {
		t.Error("another rule was limited")
	}
}

func TestOldKeysAreRemoved(t *testing.T) {
	m, now := newClock()
	for i := 0; i < 100; i++ {
		allow(t, m, fmt.Sprint(i), rule)
	}
	*now = now.Add(2 * time.Minute)
	allow(t, m, "new", rule)
	if n := len(m.entries); n != 1 {
		t.Errorf("%d keys kept", n)
	}
}

func TestAFullTableStaysBoundedAndStillLimits(t *testing.T) {
	m, now := newClock()
	m.max = 10
	for i := 0; i < 3; i++ {
		allow(t, m, "known", rule)
	}
	for i := 0; i < 50; i++ {
		if ok, _ := allow(t, m, fmt.Sprint(i), rule); !ok {
			t.Fatal("new keys must have room in the shared bucket")
		}
	}
	if n := len(m.entries); n > 11 {
		t.Errorf("%d keys, over the limit plus one shared bucket", n)
	}
	if ok, _ := allow(t, m, "known", rule); ok {
		t.Error("flooding new keys must not free a limited one")
	}
	refused := false
	for i := 0; i < 3*overflowFactor; i++ {
		if ok, _ := allow(t, m, fmt.Sprint("flood-", i), rule); !ok {
			refused = true
			break
		}
	}
	if !refused {
		t.Error("a full table must not remove the limit")
	}
	*now = now.Add(2 * time.Minute)
	allow(t, m, "after", rule)
	if _, kept := m.entries["r|after"]; !kept {
		t.Error("room must come back as keys expire")
	}
}

func TestARefundGivesOneBack(t *testing.T) {
	m, _ := newClock()
	for i := 0; i < 3; i++ {
		allow(t, m, "a", rule)
	}
	if err := m.Release(context.Background(), "a", rule); err != nil {
		t.Fatal(err)
	}
	if ok, _ := allow(t, m, "a", rule); !ok {
		t.Error("a refund must make room")
	}
	if ok, _ := allow(t, m, "a", rule); ok {
		t.Error("a refund must give back only one")
	}
	if err := m.Release(context.Background(), "never-seen", rule); err != nil {
		t.Error(err)
	}
	if _, kept := m.entries["r|never-seen"]; kept {
		t.Error("a refund must not add a key")
	}
}

func TestABurstCannotSlipPast(t *testing.T) {
	m := NewMemory()
	r := Rule{Name: "r", Limit: 10, Window: time.Minute}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 2000; j++ {
				if ok, _, _ := m.Allow(context.Background(), "a", r); ok {
					allowed.Add(1)
				}
				// Other keys make the table change size, which an unlocked map cannot survive.
				_, _, _ = m.Allow(context.Background(), fmt.Sprint(i, "-", j), r)
			}
		}()
	}
	close(start)
	wg.Wait()
	if allowed.Load() != 10 {
		t.Errorf("%d allowed", allowed.Load())
	}
}
