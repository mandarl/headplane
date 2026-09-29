// Package agent ports app/server/hp-agent.ts (the Headplane agent) into
// Go: it spawns the external agent executable, speaks its JSON-line sync
// protocol over the child's stdio, and persists the reported hostinfo into
// the shared hp_persist.db (host_info table) for the machine pages to
// read back.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tale/headplane/internal/hsapi"
)

// Defaults mirror the agentConfig schema in config-schema.ts.
const (
	DefaultHostName       = "headplane-agent"
	DefaultCacheTTL       = 3 * time.Minute
	DefaultExecutablePath = "/usr/libexec/headplane/agent"
	DefaultWorkDir        = "/var/lib/headplane/agent"
	stateFileName         = "tailscaled.state"
	maxConsecutiveErrors  = 5
)

// Config mirrors the `integration.agent` section of the Headplane config.
type Config struct {
	HostName       string
	CacheTTL       time.Duration
	ExecutablePath string
	WorkDir        string
}

// ApplyDefaults fills zero values with the schema defaults.
func (c Config) ApplyDefaults() Config {
	if c.HostName == "" {
		c.HostName = DefaultHostName
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = DefaultCacheTTL
	}
	if c.ExecutablePath == "" {
		c.ExecutablePath = DefaultExecutablePath
	}
	if c.WorkDir == "" {
		c.WorkDir = DefaultWorkDir
	}
	return c
}

// HostInfoStore is the host_info-table persistence the manager needs. It
// is satisfied by *auth.Service.
type HostInfoStore interface {
	UpsertHostInfo(map[string]json.RawMessage) error
	LookupHostInfo([]string) (map[string]json.RawMessage, error)
	PruneHostInfoNotIn([]string) (int64, error)
}

// HeadscaleAPI is the Headscale surface the manager needs: pre-auth key
// minting for first boot plus node listing/deletion for pruning.
type HeadscaleAPI interface {
	CreatePreAuthKey(user *string, ephemeral, reusable bool, expiration *time.Time, aclTags []string) (map[string]any, error)
	ListNodes() ([]hsapi.Node, error)
	DeleteNode(id string) error
}

// agentOutput is one JSON line from the agent executable.
type agentOutput struct {
	Self  string                     `json:"self"`
	Hosts map[string]json.RawMessage `json:"hosts"`
	Error string                     `json:"error"`
}

// childProc bundles the agent child process with its stdin and the pending
// sync request channel. The reader goroutine only touches its own child's
// state, so a dying reader can never steal or close a newer sync's channel.
type childProc struct {
	cmd     *exec.Cmd
	stdin   interface{ Write([]byte) (int, error) }
	pending chan string
}

// Manager is the Go port of AgentManager.
type Manager struct {
	cfg          Config
	store        HostInfoStore
	api          HeadscaleAPI
	headscaleURL string
	logger       *slog.Logger

	mu          sync.Mutex
	proc        *childProc
	disposed    bool
	isSyncing   bool
	pendingSync bool

	syncedAt          *time.Time
	nodeCount         int
	selfKey           string
	syncErr           string
	consecutiveErrors int

	ticker *time.Ticker
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewManager validates the environment (executable present and executable,
// work dir creatable) and starts the sync loop. It mirrors
// createAgentManager: an error means "Agent failed to initialize".
func NewManager(cfg Config, store HostInfoStore, api HeadscaleAPI, headscaleURL string, logger *slog.Logger) (*Manager, error) {
	cfg = cfg.ApplyDefaults()

	st, err := os.Stat(cfg.ExecutablePath)
	if err != nil {
		return nil, fmt.Errorf("agent: executable not accessible at %s: %w", cfg.ExecutablePath, err)
	}
	if st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("agent: executable not executable: %s", cfg.ExecutablePath)
	}

	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("agent: cannot create work dir %s: %w", cfg.WorkDir, err)
	}
	// Mirror the R_OK|W_OK access check.
	if f, err := os.OpenFile(filepath.Join(cfg.WorkDir, ".writetest"), os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		return nil, fmt.Errorf("agent: work dir not writable %s: %w", cfg.WorkDir, err)
	} else {
		f.Close()
		os.Remove(filepath.Join(cfg.WorkDir, ".writetest"))
	}

	m := &Manager{
		cfg:          cfg,
		store:        store,
		api:          api,
		headscaleURL: headscaleURL,
		logger:       logger.With("component", "agent"),
		done:         make(chan struct{}),
	}
	m.ticker = time.NewTicker(cfg.CacheTTL)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.sync()
		for {
			select {
			case <-m.done:
				return
			case <-m.ticker.C:
				m.sync()
			}
		}
	}()
	return m, nil
}

// Lookup returns the stored hostinfo payloads for the given node keys,
// mirroring AgentManager.lookup.
func (m *Manager) Lookup(nodeKeys []string) (map[string]json.RawMessage, error) {
	return m.store.LookupHostInfo(nodeKeys)
}

// LastSync mirrors AgentManager.lastSync.
func (m *Manager) LastSync() (syncedAt *time.Time, nodeCount int, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.syncedAt, m.nodeCount, m.syncErr
}

// AgentNodeKey mirrors AgentManager.agentNodeKey.
func (m *Manager) AgentNodeKey() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.selfKey
}

// TriggerSync runs a sync now, mirroring AgentManager.triggerSync.
func (m *Manager) TriggerSync() {
	m.sync()
}

// Dispose stops the ticker and the child process, mirroring dispose().
// The child is killed before waiting for the sync loop so a sync blocked in
// requestSync is released by the reader's exit path.
func (m *Manager) Dispose() {
	m.mu.Lock()
	if m.disposed {
		m.mu.Unlock()
		return
	}
	m.disposed = true
	proc := m.proc
	m.proc = nil
	m.mu.Unlock()

	m.ticker.Stop()
	close(m.done)
	if proc != nil && proc.cmd.Process != nil {
		_ = proc.cmd.Process.Signal(syscall.SIGTERM)
	}
	m.wg.Wait()
	if proc != nil && proc.cmd.Process != nil {
		_, _ = proc.cmd.Process.Wait()
	}
}

// generateAuthKey mints a 5-minute tag-only pre-auth key for first boot,
// mirroring generateAuthKey in hp-agent.ts.
func (m *Manager) generateAuthKey(ctx context.Context) (string, error) {
	expiration := time.Now().Add(5 * time.Minute)
	pak, err := m.api.CreatePreAuthKey(nil, false, false, &expiration, []string{"tag:" + m.cfg.HostName})
	if err != nil {
		return "", err
	}
	key, _ := pak["key"].(string)
	if key == "" {
		return "", fmt.Errorf("agent: pre-auth key response missing key")
	}
	return key, nil
}

// spawnAgent starts the executable with the tsnet environment, mirroring
// spawnAgent in hp-agent.ts.
func (m *Manager) spawnAgent(authKey string) (*childProc, error) {
	cmd := exec.Command(m.cfg.ExecutablePath)
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"HEADPLANE_AGENT_WORK_DIR=" + m.cfg.WorkDir,
		"HEADPLANE_AGENT_TS_SERVER=" + m.headscaleURL,
		"HEADPLANE_AGENT_HOSTNAME=" + m.cfg.HostName,
		"HEADPLANE_AGENT_DEBUG=" + boolStr(m.logger.Enabled(context.Background(), slog.LevelDebug)),
	}
	if authKey != "" {
		cmd.Env = append(cmd.Env, "HEADPLANE_AGENT_TS_AUTHKEY="+authKey)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	cp := &childProc{cmd: cmd, stdin: stdin}
	m.mu.Lock()
	m.proc = cp
	m.mu.Unlock()

	// Drain stderr to the debug log, like the TS 'data' handler.
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if text := strings.TrimSpace(sc.Text()); text != "" {
				m.logger.Debug("agent stderr", "line", text)
			}
		}
	}()

	// Deliver stdout lines to the pending sync, like the TS readline
	// handler; on child exit, fail the pending sync. The reader only touches
	// its own child's pending channel, so a dying reader can never steal or
	// close a newer sync's channel after a respawn.
	go func() {
		sc := bufio.NewScanner(stdout)
		// The hostinfo payload can be large; allow up to 64 MiB per line.
		sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for sc.Scan() {
			m.mu.Lock()
			pending := cp.pending
			cp.pending = nil
			m.mu.Unlock()
			if pending != nil {
				pending <- sc.Text()
			}
		}
		m.mu.Lock()
		pending := cp.pending
		cp.pending = nil
		disposed := m.disposed
		m.mu.Unlock()
		// Reap the child so ProcessState is set (ensureProcess uses it for
		// liveness) and no zombie is left behind.
		_ = cp.cmd.Wait()
		if !disposed {
			m.logger.Warn("agent process exited")
		}
		if pending != nil {
			close(pending)
		}
	}()

	return cp, nil
}

// ensureProcess mirrors ensureProcess: reuse the running child, else spawn
// one (minting a pre-auth key unless tsnet state already exists).
func (m *Manager) ensureProcess(ctx context.Context) (*childProc, error) {
	m.mu.Lock()
	proc := m.proc
	m.mu.Unlock()
	if proc != nil && proc.cmd.ProcessState == nil {
		return proc, nil
	}
	if _, err := os.Stat(filepath.Join(m.cfg.WorkDir, stateFileName)); err == nil {
		m.logger.Debug("reusing existing tsnet identity")
		return m.spawnAgent("")
	}
	m.logger.Info("no tsnet state found, generating pre-auth key")
	key, err := m.generateAuthKey(ctx)
	if err != nil {
		return nil, err
	}
	return m.spawnAgent(key)
}

// requestSync writes "sync\n" and awaits the single JSON response line,
// mirroring sendSync/requestSync.
func (m *Manager) requestSync(proc *childProc) (agentOutput, error) {
	pending := make(chan string, 1)
	m.mu.Lock()
	proc.pending = pending
	stdin := proc.stdin
	m.mu.Unlock()

	if _, err := stdin.Write([]byte("sync\n")); err != nil {
		return agentOutput{}, err
	}
	line, ok := <-pending
	if !ok || line == "" {
		return agentOutput{}, fmt.Errorf("agent: process closed unexpectedly")
	}
	var out agentOutput
	if err := json.Unmarshal([]byte(line), &out); err != nil {
		return agentOutput{}, fmt.Errorf("agent: bad sync response: %w", err)
	}
	return out, nil
}

// sync mirrors sync() in hp-agent.ts, including the isSyncing/pendingResync
// coalescing and the 5-consecutive-error state reset.
func (m *Manager) sync() {
	m.mu.Lock()
	if m.isSyncing {
		m.pendingSync = true
		m.logger.Debug("sync already in progress, queued resync")
		m.mu.Unlock()
		return
	}
	m.isSyncing = true
	m.mu.Unlock()

	m.doSync()

	for {
		m.mu.Lock()
		m.isSyncing = false
		again := m.pendingSync
		m.pendingSync = false
		m.mu.Unlock()
		if !again {
			return
		}
		m.mu.Lock()
		m.isSyncing = true
		m.mu.Unlock()
		m.doSync()
	}
}

func (m *Manager) doSync() {
	ctx := context.Background()
	proc, err := m.ensureProcess(ctx)
	if err != nil {
		m.failSync(fmt.Sprintf("ensure process: %v", err))
		return
	}
	out, err := m.requestSync(proc)
	if err != nil {
		// The child died; drop it so the next sync respawns instead of
		// reusing a dead process.
		m.mu.Lock()
		if m.proc == proc {
			m.proc = nil
		}
		m.mu.Unlock()
		m.failSync(err.Error())
		return
	}
	if out.Error != "" {
		m.failSync(out.Error)
		return
	}

	m.mu.Lock()
	m.consecutiveErrors = 0
	m.mu.Unlock()

	if out.Hosts == nil {
		out.Hosts = map[string]json.RawMessage{}
	}
	if err := m.store.UpsertHostInfo(out.Hosts); err != nil {
		m.logger.Error("upsert hostinfo failed", "error", err)
	}
	m.pruneStaleHostInfo()
	m.pruneEphemeralNodes()

	now := time.Now()
	m.mu.Lock()
	m.syncedAt = &now
	m.nodeCount = len(out.Hosts)
	m.selfKey = out.Self
	m.syncErr = ""
	m.mu.Unlock()
	m.logger.Info("sync complete", "nodes", len(out.Hosts))
}

// failSync records a sync failure and, after 5 consecutive errors, kills
// the child and clears tsnet state so the next attempt starts fresh.
func (m *Manager) failSync(msg string) {
	m.mu.Lock()
	m.consecutiveErrors++
	n := m.consecutiveErrors
	m.syncErr = msg
	m.mu.Unlock()

	m.logger.Error("sync failed", "attempt", n, "error", msg)
	if n >= maxConsecutiveErrors {
		m.logger.Warn("too many consecutive failures, killing agent and clearing state")
		m.mu.Lock()
		proc := m.proc
		m.proc = nil
		m.mu.Unlock()
		if proc != nil && proc.cmd.Process != nil {
			_ = proc.cmd.Process.Signal(syscall.SIGTERM)
		}
		_ = os.Remove(filepath.Join(m.cfg.WorkDir, stateFileName))
	}
}

// pruneStaleHostInfo mirrors pruneStaleHostInfo: drop host_info rows for
// nodes Headscale no longer lists.
func (m *Manager) pruneStaleHostInfo() {
	nodes, err := m.api.ListNodes()
	if err != nil {
		m.logger.Debug("failed to prune stale hostinfo", "error", err)
		return
	}
	keys := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if k, ok := n["nodeKey"].(string); ok && k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	if deleted, err := m.store.PruneHostInfoNotIn(keys); err != nil {
		m.logger.Debug("failed to prune stale hostinfo", "error", err)
	} else if deleted > 0 {
		m.logger.Info("pruned stale hostinfo entries", "count", deleted)
	}
}

// pruneEphemeralNodes mirrors pruneEphemeralNodes: delete offline nodes
// whose pre-auth key was ephemeral (Headscale bug workaround).
func (m *Manager) pruneEphemeralNodes() {
	nodes, err := m.api.ListNodes()
	if err != nil {
		m.logger.Debug("failed to prune ephemeral nodes", "error", err)
		return
	}
	for _, n := range nodes {
		pak, _ := n["preAuthKey"].(map[string]any)
		ephemeral, _ := pak["ephemeral"].(bool)
		online, _ := n["online"].(bool)
		if !ephemeral || online {
			continue
		}
		id, _ := n["id"].(string)
		name, _ := n["givenName"].(string)
		if id == "" {
			continue
		}
		if err := m.api.DeleteNode(id); err != nil {
			m.logger.Debug("failed to prune ephemeral node", "node", name, "error", err)
			continue
		}
		m.logger.Info("pruned offline ephemeral node", "node", name)
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
