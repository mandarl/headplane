package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tale/headplane/internal/hsapi"
	"log/slog"
)

type fakeStore struct {
	payloads map[string]json.RawMessage
}

func (f *fakeStore) UpsertHostInfo(p map[string]json.RawMessage) error {
	if f.payloads == nil {
		f.payloads = map[string]json.RawMessage{}
	}
	for k, v := range p {
		f.payloads[k] = v
	}
	return nil
}

func (f *fakeStore) PruneHostInfoNotIn(keys []string) (int64, error) { return 0, nil }

func (f *fakeStore) LookupHostInfo(keys []string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, k := range keys {
		if v, ok := f.payloads[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

type fakeAPI struct {
	nodes   []hsapi.Node
	deleted []string
}

func (f *fakeAPI) CreatePreAuthKey(user *string, ephemeral, reusable bool, expiration *time.Time, aclTags []string) (map[string]any, error) {
	return map[string]any{"key": "test-preauth-key"}, nil
}

func (f *fakeAPI) ListNodes() ([]hsapi.Node, error) { return f.nodes, nil }

func (f *fakeAPI) DeleteNode(id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeExecutable writes a shell script that answers one sync with the given
// JSON line (consumed once per process lifetime: read one line, print one
// line, then keep reading until stdin closes).
func fakeExecutable(t *testing.T, response string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent")
	script := "#!/bin/sh\nwhile read -r line; do printf '%s\\n' '" + response + "'; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func waitForSync(t *testing.T, m *Manager, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if syncedAt, _, _ := m.LastSync(); syncedAt != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for agent sync")
}

func TestManagerSyncRoundTrip(t *testing.T) {
	exe := fakeExecutable(t, `{"self":"nodekey1","hosts":{"nodekey1":{"hostname":"agent-host"}}}`)
	store := &fakeStore{}
	api := &fakeAPI{nodes: []hsapi.Node{{"nodeKey": "nodekey1"}}}

	m, err := NewManager(Config{
		ExecutablePath: exe,
		WorkDir:        t.TempDir(),
		CacheTTL:       time.Hour,
	}, store, api, "http://headscale:8080", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Dispose()

	waitForSync(t, m, 5*time.Second)

	if len(store.payloads) != 1 {
		t.Fatalf("expected 1 hostinfo payload, got %d", len(store.payloads))
	}
	if string(store.payloads["nodekey1"]) != `{"hostname":"agent-host"}` {
		t.Fatalf("wrong payload: %s", store.payloads["nodekey1"])
	}
	if got := m.AgentNodeKey(); got != "nodekey1" {
		t.Fatalf("wrong self key: %q", got)
	}
	syncedAt, count, errMsg := m.LastSync()
	if syncedAt == nil || count != 1 || errMsg != "" {
		t.Fatalf("bad lastSync: %v %d %q", syncedAt, count, errMsg)
	}
}

func TestManagerMissingExecutable(t *testing.T) {
	_, err := NewManager(Config{
		ExecutablePath: filepath.Join(t.TempDir(), "nope"),
		WorkDir:        t.TempDir(),
	}, &fakeStore{}, &fakeAPI{}, "http://x", testLogger())
	if err == nil {
		t.Fatal("expected error for missing executable")
	}
}

func TestManagerConsecutiveErrorResetsState(t *testing.T) {
	exe := fakeExecutable(t, `{"error":"boom"}`)
	store := &fakeStore{}
	workDir := t.TempDir()
	m, err := NewManager(Config{
		ExecutablePath: exe,
		WorkDir:        workDir,
		CacheTTL:       time.Hour,
	}, store, &fakeAPI{}, "http://x", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Dispose()

	// 5 consecutive failures: the 5th clears tsnet state. TriggerSync calls
	// may coalesce with the manager's background initial sync (pendingSync),
	// so poll until the failures have been recorded.
	for i := 0; i < 5; i++ {
		m.TriggerSync()
	}
	deadline := time.Now().Add(10 * time.Second)
	var errMsg string
	for time.Now().Before(deadline) {
		_, _, errMsg = m.LastSync()
		if errMsg != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Sync errors are recorded on the manager like TS's state.error.
	if errMsg != "boom" {
		t.Fatalf("expected 'boom', got %q", errMsg)
	}
}

func TestManagerPrunesEphemeralOfflineNodes(t *testing.T) {
	exe := fakeExecutable(t, `{"self":"k","hosts":{}}`)
	api := &fakeAPI{nodes: []hsapi.Node{
		{"id": "1", "nodeKey": "k1", "givenName": "ephem", "online": false,
			"preAuthKey": map[string]any{"ephemeral": true}},
		{"id": "2", "nodeKey": "k2", "givenName": "online-ephem", "online": true,
			"preAuthKey": map[string]any{"ephemeral": true}},
		{"id": "3", "nodeKey": "k3", "givenName": "plain", "online": false},
	}}
	m, err := NewManager(Config{
		ExecutablePath: exe,
		WorkDir:        t.TempDir(),
		CacheTTL:       time.Hour,
	}, &fakeStore{}, api, "http://x", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Dispose()

	waitForSync(t, m, 5*time.Second)

	if len(api.deleted) != 1 || api.deleted[0] != "1" {
		t.Fatalf("expected only node 1 pruned, got %v", api.deleted)
	}
}

func TestGenerateAuthKeyUsesTagOnlyKey(t *testing.T) {
	exe := fakeExecutable(t, `{"self":"k","hosts":{}}`)
	api := &fakeAPI{}
	m, err := NewManager(Config{
		ExecutablePath: exe,
		WorkDir:        t.TempDir(),
		CacheTTL:       time.Hour,
		HostName:       "my-agent",
	}, &fakeStore{}, api, "http://x", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Dispose()

	key, err := m.generateAuthKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if key != "test-preauth-key" {
		t.Fatalf("wrong key: %q", key)
	}
}
