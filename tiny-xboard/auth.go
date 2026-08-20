package main

import (
	"net/http"
	"strconv"
)

// authenticate verifies the UniProxy request credentials in a single consistent
// node snapshot: node_id must resolve to an enabled managed node, the token
// must match (per-node override, else the shared panel token) and the requested
// node_type must be served. It writes the HTTP error response and returns
// (NodeEntry{}, false) on failure, or the matched entry and true.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (NodeEntry, bool) {
	id, err := strconv.Atoi(r.URL.Query().Get("node_id"))
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Server does not exist"})
		return NodeEntry{}, false
	}
	snap := s.store.nodeSnapshot(id)
	if !snap.Found || !snap.Enabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Server does not exist"})
		return NodeEntry{}, false
	}
	if !s.store.validToken(r.URL.Query().Get("token"), id) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid token"})
		return NodeEntry{}, false
	}
	if !matchNodeType(snap.Type, r.URL.Query().Get("node_type")) {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Server does not exist"})
		return NodeEntry{}, false
	}
	entry, _ := s.store.lookupNode(id)
	return entry, true
}

// matchNodeType reports whether the requested node_type is served by a node
// configured with cfgType. "both" nodes answer any type; otherwise the
// normalized types must be equal.
func matchNodeType(cfgType, requested string) bool {
	cfg := normalizeNodeType(cfgType)
	if cfg == "both" {
		return true
	}
	return cfg == normalizeNodeType(requested)
}
