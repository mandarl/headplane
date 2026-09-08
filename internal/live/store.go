// Package live ports app/server/headscale/live-store.ts into Go.
//
// It is a concurrency-safe, in-memory cache of Headscale resources (nodes,
// users) with:
//   - versioned snapshots: every change bumps one shared monotonic counter,
//     exposed as decimal strings exactly like the TS versions;
//   - change detection by canonical JSON comparison (no notify when the
//     payload is identical);
//   - background polling per resource (nodes 5s, users 10s — the TS polled
//     users every 15s; the tighter 10s bound meets the ≤10s staleness
//     budget for both resources and SSE subscribers);
//   - a bounded replay ring plus monotonic event IDs so the SSE endpoint can
//     honor Last-Event-ID resumption and dedupe;
//   - bounded, non-blocking subscribers: a slow consumer is evicted (its
//     SSE stream ends and the client reconnects fresh) instead of stalling
//     the poll loop.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tale/headplane/internal/hsapi"
)

// ErrNoClient is returned when the store has no Headscale client to fetch
// with (no default API key configured and no request has supplied one yet).
var ErrNoClient = errors.New("live: no Headscale client available")

// Resource defines a pollable Headscale resource.
type Resource struct {
	Key          string
	PollInterval time.Duration
	Fetch        func(ctx context.Context, c *hsapi.Client) (any, error)
}

// NodesResource mirrors nodesResource: the Headscale node list.
var NodesResource = Resource{
	Key:          "nodes",
	PollInterval: 5 * time.Second,
	Fetch: func(ctx context.Context, c *hsapi.Client) (any, error) {
		return c.ListNodes()
	},
}

// UsersResource mirrors usersResource: the Headscale user list. The TS
// polled every 15s; this port uses 10s to meet the ≤10s staleness budget.
var UsersResource = Resource{
	Key:          "users",
	PollInterval: 10 * time.Second,
	Fetch: func(ctx context.Context, c *hsapi.Client) (any, error) {
		return c.ListUsers("", "", "")
	},
}

// Snapshot is the cached value of one resource.
type Snapshot struct {
	Data      any
	Version   string
	FetchedAt time.Time
}

// Event is a versioned change notification. ID is globally monotonic and
// doubles as the SSE event id for Last-Event-ID resumption.
type Event struct {
	ID       uint64
	Resource string
	Version  string
}

// replayCap bounds the event replay ring.
const replayCap = 64

// subCap bounds each subscriber's pending queue; overflow evicts.
const subCap = 32

type snapshot struct {
	data      any
	raw       string
	version   string
	fetchedAt time.Time
}

// Store is the live store. It is safe for concurrent use.
type Store struct {
	mu        sync.Mutex
	snaps     map[string]*snapshot
	subs      map[uint64]chan Event
	nextSub   uint64
	ring      []Event // bounded replay ring, oldest-first
	resources map[string]Resource

	counter atomic.Uint64 // shared version/event counter, starts at 0
	client  atomic.Pointer[hsapi.Client]
	polling map[string]context.CancelFunc

	logger *slog.Logger
}

// NewStore creates a store. client may be nil; SetClient installs or
// replaces the default Headscale client (e.g. after /version detection or
// capability updates).
func NewStore(logger *slog.Logger, client *hsapi.Client) *Store {
	s := &Store{
		snaps:     map[string]*snapshot{},
		subs:      map[uint64]chan Event{},
		resources: map[string]Resource{},
		polling:   map[string]context.CancelFunc{},
		logger:    logger.With("component", "live"),
	}
	if client != nil {
		s.client.Store(client)
	}
	return s
}

// SetClient replaces the default client used by the poll loops.
func (s *Store) SetClient(c *hsapi.Client) {
	if c != nil {
		s.client.Store(c)
	}
}

// Get mirrors LiveStore.get: returns the cached snapshot, fetching once
// when nothing is cached yet, and ensures background polling is running.
// The supplied client becomes the polling client (mirroring
// storedApiClient), when non-nil.
func (s *Store) Get(ctx context.Context, r Resource, c *hsapi.Client) (Snapshot, error) {
	if c != nil {
		s.client.Store(c)
	}
	s.mu.Lock()
	if _, ok := s.resources[r.Key]; !ok {
		s.resources[r.Key] = r
	}
	snap, ok := s.snaps[r.Key]
	s.mu.Unlock()
	if !ok {
		if err := s.fetch(ctx, r, c); err != nil {
			return Snapshot{}, err
		}
		s.mu.Lock()
		snap = s.snaps[r.Key]
		s.mu.Unlock()
	}
	s.ensurePolling(r)
	return Snapshot{Data: snap.data, Version: snap.version, FetchedAt: snap.fetchedAt}, nil
}

// Refresh mirrors LiveStore.refresh: forces a fetch of the resource,
// notifying subscribers when the payload changed.
func (s *Store) Refresh(ctx context.Context, r Resource, c *hsapi.Client) error {
	if c != nil {
		s.client.Store(c)
	}
	s.mu.Lock()
	if _, ok := s.resources[r.Key]; !ok {
		s.resources[r.Key] = r
	}
	s.mu.Unlock()
	return s.fetch(ctx, r, c)
}

// Versions mirrors getVersions: the current version per resource.
func (s *Store) Versions() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.snaps))
	for k, snap := range s.snaps {
		out[k] = snap.version
	}
	return out
}

// Subscribe registers a change listener. The returned channel receives
// change events; it is closed when the subscriber is evicted (slow
// consumer) or the store is disposed. The caller must call the returned
// function to unsubscribe.
func (s *Store) Subscribe() (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSub++
	id := s.nextSub
	ch := make(chan Event, subCap)
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(ch)
		}
	}
}

// Replay returns buffered events with ID greater than since, oldest first,
// for Last-Event-ID resumption. It is bounded by the replay ring.
func (s *Store) Replay(since uint64) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.ring {
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out
}

// Dispose stops all poll loops and closes every subscriber channel,
// mirroring LiveStore.dispose.
func (s *Store) Dispose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, cancel := range s.polling {
		cancel()
		delete(s.polling, key)
	}
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	s.snaps = map[string]*snapshot{}
	s.ring = nil
}

// clientFor resolves the effective client for a fetch: the explicit one,
// else the stored default.
func (s *Store) clientFor(c *hsapi.Client) *hsapi.Client {
	if c != nil {
		return c
	}
	return s.client.Load()
}

// fetch performs one fetch cycle: on payload change it bumps the shared
// version counter, stores the snapshot, appends to the replay ring, and
// notifies subscribers (non-blocking; slow consumers are evicted).
func (s *Store) fetch(ctx context.Context, r Resource, c *hsapi.Client) error {
	client := s.clientFor(c)
	if client == nil {
		return ErrNoClient
	}
	data, err := r.Fetch(ctx, client)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.snaps[r.Key]; ok && prev.raw == string(raw) {
		s.logger.Debug("resource unchanged", "resource", r.Key)
		return nil
	}
	_, hadPrev := s.snaps[r.Key]
	id := s.counter.Add(1)
	ver := itoa(id)
	s.snaps[r.Key] = &snapshot{data: data, raw: string(raw), version: ver, fetchedAt: time.Now()}
	s.logger.Debug("resource updated", "resource", r.Key, "version", ver)

	ev := Event{ID: id, Resource: r.Key, Version: ver}
	// The initial fetch stores the snapshot but does not notify listeners
	// and does not enter the replay ring: the SSE hello already carries
	// the current version map, so there is nothing to resume.
	if !hadPrev {
		return nil
	}
	s.appendRing(ev)
	s.broadcastLocked(ev)
	return nil
}

// appendRing appends to the bounded replay ring.
func (s *Store) appendRing(ev Event) {
	s.ring = append(s.ring, ev)
	if len(s.ring) > replayCap {
		s.ring = s.ring[len(s.ring)-replayCap:]
	}
}

// broadcastLocked delivers ev to every subscriber without blocking; a
// subscriber whose queue is full is evicted (channel closed) so one slow
// SSE consumer can never stall the poll loop or grow memory unboundedly.
func (s *Store) broadcastLocked(ev Event) {
	for id, ch := range s.subs {
		select {
		case ch <- ev:
		default:
			s.logger.Warn("evicting slow subscriber", "subscriber", id, "resource", ev.Resource)
			delete(s.subs, id)
			close(ch)
		}
	}
}

// ensurePolling starts the background poll loop for a resource once,
// mirroring ensurePolling. Poll failures are logged, never fatal.
func (s *Store) ensurePolling(r Resource) {
	s.mu.Lock()
	if _, ok := s.polling[r.Key]; ok {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.polling[r.Key] = cancel
	s.mu.Unlock()

	s.logger.Debug("started polling", "resource", r.Key, "interval", r.PollInterval)
	go func() {
		t := time.NewTicker(r.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				client := s.client.Load()
				if client == nil {
					continue
				}
				if err := s.fetch(ctx, r, client); err != nil {
					s.logger.Error("failed to poll resource", "resource", r.Key, "error", err)
				}
			}
		}
	}()
}

func itoa(n uint64) string {
	var buf [20]byte
	i := len(buf)
	for n >= 10 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	i--
	buf[i] = byte('0' + n)
	return string(buf[i:])
}
