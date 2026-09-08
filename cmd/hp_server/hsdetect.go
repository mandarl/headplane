package main

// detectHeadscaleVersion mirrors the detect() loop in
// app/server/headscale/api/index.ts: probe the unauthenticated /version
// endpoint (present since Headscale 0.27.0), derive the capability flags,
// and install a default client with those capabilities on the live store.
// Until detection succeeds the flags stay capabilities-permissive; failures
// retry every 30 s. A 404 means the server predates 0.27.0 and is below the
// supported floor.

import (
	"errors"
	"time"

	"github.com/tale/headplane/internal/hsapi"
)

// versionRetryInterval mirrors the TS 30 s detect retry.
const versionRetryInterval = 30 * time.Second

func (s *Server) detectHeadscaleVersion(apiKey string, done <-chan struct{}) {
	detectOnce := func() bool {
		v, err := hsapi.DetectVersion(s.cfg.Headscale.URL, s.cfg.Headscale.TLSCertPath, s.logger)
		if err != nil {
			var apiErr *hsapi.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
				s.logger.Error("Headscale version detection failed: /version not found; Headscale 0.27.0 or later is required")
			} else {
				s.logger.Warn("Headscale version detection failed; retrying", "error", err)
			}
			return false
		}
		caps := hsapi.CapabilitiesFor(v)
		s.setHsCaps(caps)
		if apiKey != "" {
			s.liveStore.SetClient(hsapi.NewClient(s.cfg.Headscale.URL, apiKey,
				s.cfg.Headscale.TLSCertPath, caps, s.logger))
		}
		// Phase 5: record the version for /api/info and lazily build the
		// agent manager (it needs the detected 0.28+ capability).
		s.onVersionDetected(v, caps, apiKey)
		s.logger.Info("detected Headscale version", "version", v.Raw,
			"preAuthKeysHaveStableIds", caps.PreAuthKeysHaveStableIds,
			"nodeTagsAreFlat", caps.NodeTagsAreFlat,
			"nodeOwnerIsImmutable", caps.NodeOwnerIsImmutable)
		return true
	}

	if detectOnce() {
		return
	}
	ticker := time.NewTicker(versionRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if detectOnce() {
				return
			}
		}
	}
}
