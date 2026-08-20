package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"
)

// Body-memory hardening for 64MB-class containers. The per-request push body is
// capped at maxPushBodySize, and at most maxConcurrentBodyReads bodies are read
// concurrently, so total in-flight body memory is bounded by
// maxConcurrentBodyReads * maxPushBodySize (≈2 MB) no matter how many large
// payloads arrive at once. The real mini-sb-agent sends ≈30 KB per push, far
// below the cap. Saturated readers return 503 instead of queuing unboundedly.
const (
	maxPushBodySize        = 256 << 10
	maxConcurrentBodyReads = 8
	bodyReadTimeout        = 2 * time.Second
)

var pushBodySem = make(chan struct{}, maxConcurrentBodyReads)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "method not allowed"})
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/server/UniProxy/user", s.handleUser)
	mux.HandleFunc("/api/v1/server/UniProxy/config", s.handleConfig)
	mux.HandleFunc("/api/v1/server/UniProxy/push", s.handlePush)
	return mux
}

// handleUser returns the enabled user list in the Xboard UniProxy format.
// Responds to: GET /api/v1/server/UniProxy/user?token=..&node_id=..&node_type=..
func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	if err := s.store.RefreshUsers(); err != nil {
		log.Printf("[warn] refresh users: %v", err)
	}
	users := s.store.EnabledUsers()
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// handleConfig returns the node configuration in the Xboard UniProxy format.
// Responds to: GET /api/v1/server/UniProxy/config?token=..&node_id=..&node_type=..
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	entry, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, NodeConfigResponse(entry))
}

// handlePush accepts a cumulative increment map {user_id: [upload, download]}
// and acculates it into the in-memory traffic store. Unknown user ids are
// ignored without failing the whole push.
// Responds to: POST /api/v1/server/UniProxy/push?token=..&node_id=..&node_type=..
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	_, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	// Bound the per-request body size and the number of concurrent readers so a
	// flood of large bodies can never exhaust memory (see constants above).
	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodySize)
	select {
	case pushBodySem <- struct{}{}:
	case <-time.After(bodyReadTimeout):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": "server busy"})
		return
	}
	defer func() { <-pushBodySem }()
	if err := s.store.RefreshUsers(); err != nil {
		log.Printf("[warn] refresh users: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"message": "payload too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "invalid payload"})
		return
	}

	accepted := 0
	// Xboard UniProxy real format:
	//   {"traffic":[{"uid":1,"up":100,"down":100,"created_at":1712000000}]}
	if entriesRaw, ok := raw["traffic"]; ok {
		var entries []struct {
			UID  int   `json:"uid"`
			Up   int64 `json:"up"`
			Down int64 `json:"down"`
		}
		if err := json.Unmarshal(entriesRaw, &entries); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "invalid payload"})
			return
		}
		for _, e := range entries {
			if s.store.Accumulate(e.UID, e.Up, e.Down) {
				accepted++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "accepted": accepted})
		return
	}

	// Compact form: {"<uid>":[upload,download]}, kept for simple clients/curl.
	for idStr, v := range raw {
		var pair [2]int64
		if err := json.Unmarshal(v, &pair); err != nil {
			continue
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		if s.store.Accumulate(id, pair[0], pair[1]) {
			accepted++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "accepted": accepted})
}

// NodeConfigResponse renders one node entry as the Xboard UniProxy NodeConfig
// the mini-sb-agent understands (protocol/server_port/tls_settings/base_config...).
func NodeConfigResponse(e NodeEntry) map[string]any {
	port := e.Server.Port
	if port <= 0 {
		port = 443
	}
	out := map[string]any{}

	switch normalizeNodeType(e.Type) {
	case "vless":
		out["protocol"] = "vless"
		out["listen_ip"] = e.Server.Listen
		out["server_port"] = port
		out["network"] = firstNonEmpty(e.Protocol.Network, "tcp")
		if e.Protocol.Flow != "" {
			out["flow"] = e.Protocol.Flow
		}
		if e.Protocol.Decryption != "" {
			out["decryption"] = e.Protocol.Decryption
		}
		tlsInt := 0
		if e.TLS.Enabled {
			tlsInt = 2 // reality
		}
		out["tls"] = tlsInt

		ts := map[string]any{}
		if e.TLS.ServerName != "" {
			ts["server_name"] = e.TLS.ServerName
		}
		if sp := e.TLS.ServerPort; sp > 0 {
			ts["server_port"] = strconv.Itoa(sp)
		}
		if e.TLS.PublicKey != "" {
			ts["public_key"] = e.TLS.PublicKey
		}
		if e.TLS.PrivateKey != "" {
			ts["private_key"] = e.TLS.PrivateKey
		}
		if e.TLS.ShortID != "" {
			ts["short_id"] = e.TLS.ShortID
		}
		if e.TLS.AllowInsecure {
			ts["allow_insecure"] = true
		}
		if len(ts) > 0 {
			out["tls_settings"] = ts
		}
	case "hy2":
		out["protocol"] = "hysteria"
		out["version"] = 2
		out["server_port"] = port
		out["listen_ip"] = e.Server.Listen

		ts := map[string]any{}
		if e.TLS.ServerName != "" {
			out["server_name"] = e.TLS.ServerName
			ts["server_name"] = e.TLS.ServerName
		}
		if e.TLS.AllowInsecure {
			ts["allow_insecure"] = true
		}
		if len(ts) > 0 {
			out["tls_settings"] = ts
		}
	}

	syncInt := e.Runtime.SyncInterval
	if syncInt <= 0 {
		syncInt = DefaultSyncInterval
	}
	out["base_config"] = map[string]any{
		"push_interval": syncInt,
		"pull_interval": syncInt,
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
