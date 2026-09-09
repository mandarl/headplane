package live

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tale/headplane/internal/hsapi"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubResource returns a Resource whose fetch yields successive payloads.
// The call counter is atomic because the store may invoke Fetch
// concurrently.
func stubResource(key string, interval time.Duration, payloads ...any) (Resource, *atomic.Int64) {
	var calls atomic.Int64
	return Resource{
		Key:          key,
		PollInterval: interval,
		Fetch: func(ctx context.Context, c *hsapi.Client) (any, error) {
			n := calls.Add(1) - 1
			return payloads[int(n)%len(payloads)], nil
		},
	}, &calls
}

func TestGetCachesAndVersions(t *testing.T) {
	res, calls := stubResource("nodes", time.Hour, []string{"a"})
	s := NewStore(testLogger(), hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, testLogger()))
	defer s.Dispose()

	snap1, err := s.Get(context.Background(), res, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap2, err := s.Get(context.Background(), res, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("fetch called %d times, want 1 (cached)", calls.Load())
	}
	if snap1.Version != snap2.Version || snap1.Version == "" {
		t.Fatalf("versions = %q %q", snap1.Version, snap2.Version)
	}
	if v := s.Versions()["nodes"]; v != snap1.Version {
		t.Fatalf("Versions() = %q", v)
	}
}

func TestInitialFetchDoesNotNotify(t *testing.T) {
	res, _ := stubResource("nodes", time.Hour, []string{"a"})
	s := NewStore(testLogger(), hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, testLogger()))
	defer s.Dispose()

	ch, unsub := s.Subscribe()
	defer unsub()
	if _, err := s.Get(context.Background(), res, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("initial fetch notified: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestChangeNotifiesAndDedupes(t *testing.T) {
	res, _ := stubResource("nodes", time.Hour, []string{"a"}, []string{"a"}, []string{"b"})
	s := NewStore(testLogger(), hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, testLogger()))
	defer s.Dispose()

	ch, unsub := s.Subscribe()
	defer unsub()
	if _, err := s.Get(context.Background(), res, nil); err != nil { // "a", no notify
		t.Fatal(err)
	}
	if err := s.Refresh(context.Background(), res, nil); err != nil { // "a" again
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("unchanged payload notified: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	if err := s.Refresh(context.Background(), res, nil); err != nil { // "b"
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Resource != "nodes" || ev.Version == "" || ev.ID == 0 {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("changed payload did not notify")
	}
}

func TestSharedVersionCounter(t *testing.T) {
	ra, _ := stubResource("nodes", time.Hour, []string{"a"})
	rb, _ := stubResource("users", time.Hour, []string{"u"})
	s := NewStore(testLogger(), hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, testLogger()))
	defer s.Dispose()
	sna, _ := s.Get(context.Background(), ra, nil)
	snb, _ := s.Get(context.Background(), rb, nil)
	if sna.Version == snb.Version {
		t.Fatalf("versions share a counter, got %q == %q", sna.Version, snb.Version)
	}
}

func TestReplay(t *testing.T) {
	res, _ := stubResource("nodes", time.Hour, []string{"a"}, []string{"b"}, []string{"c"})
	s := NewStore(testLogger(), hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, testLogger()))
	defer s.Dispose()
	if _, err := s.Get(context.Background(), res, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(context.Background(), res, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(context.Background(), res, nil); err != nil {
		t.Fatal(err)
	}
	all := s.Replay(0)
	// Only real changes enter the replay ring; the initial snapshot is
	// covered by the SSE hello's version map.
	if len(all) != 2 {
		t.Fatalf("replay(0) = %d events, want 2", len(all))
	}
	rest := s.Replay(all[0].ID)
	if len(rest) != 1 || rest[0].Version != all[1].Version {
		t.Fatalf("replay(since) = %+v", rest)
	}
	if got := s.Replay(all[1].ID); len(got) != 0 {
		t.Fatalf("replay(latest) = %+v", got)
	}
}

func TestSlowSubscriberEvicted(t *testing.T) {
	res, _ := stubResource("nodes", time.Hour, []string{"a"})
	s := NewStore(testLogger(), hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, testLogger()))
	defer s.Dispose()
	if _, err := s.Get(context.Background(), res, nil); err != nil {
		t.Fatal(err)
	}
	ch, _ := s.Subscribe() // never drained, never unsubscribed
	// Overflow its queue: each Refresh with a new payload broadcasts.
	for i := 0; i < subCap+5; i++ {
		payload := []string{"a", string(rune('b' + i))}
		r := Resource{Key: "nodes", PollInterval: time.Hour,
			Fetch: func(ctx context.Context, c *hsapi.Client) (any, error) { return payload, nil }}
		if err := s.Refresh(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Drain: the evicted channel is closed but may still hold buffered
	// events; a closed channel is what proves eviction.
	drained := 0
	closed := false
	timeout := time.After(2 * time.Second)
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			} else {
				drained++
			}
		case <-timeout:
			t.Fatal("slow subscriber was not evicted")
		}
	}
	if drained == 0 {
		t.Fatal("expected some buffered events before eviction closed the channel")
	}
}

func TestErrNoClient(t *testing.T) {
	res, _ := stubResource("nodes", time.Hour, []string{"a"})
	s := NewStore(testLogger(), nil)
	defer s.Dispose()
	if _, err := s.Get(context.Background(), res, nil); !errors.Is(err, ErrNoClient) {
		t.Fatalf("err = %v, want ErrNoClient", err)
	}
}

func TestConcurrentGet(t *testing.T) {
	res, _ := stubResource("nodes", 5*time.Millisecond, []string{"a"})
	s := NewStore(testLogger(), hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, testLogger()))
	defer s.Dispose()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Get(context.Background(), res, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestDisposeClosesSubscribers(t *testing.T) {
	s := NewStore(testLogger(), hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, testLogger()))
	ch, _ := s.Subscribe()
	s.Dispose()
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel should be closed after Dispose")
	}
}
