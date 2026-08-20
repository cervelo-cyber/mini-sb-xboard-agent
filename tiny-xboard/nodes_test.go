package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const sharedToken = "SHARED-TOKEN-abcdef123456"

func writeNodesJSON(t *testing.T, dir string, nf NodesFile) {
	t.Helper()
	data, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nodes.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func multiNodeFile(overrides ...NodeEntry) NodesFile {
	nodes := []NodeEntry{
		{
			ID: 1, Name: "nat-vless-01", Type: "vless", Enabled: true,
			Server:   ServerCfg{Listen: "0.0.0.0", Port: 443},
			Protocol: ProtocolCfg{Type: "vless", Network: "tcp", Flow: "xtls-rprx-vision"},
			TLS:      TLSCfg{Enabled: true, ServerName: "www.microsoft.com", ServerPort: 443, ShortID: "abcd1234"},
		},
		{
			ID: 2, Name: "nat-hy2-01", Type: "hy2", Enabled: true,
			Server: ServerCfg{Listen: "0.0.0.0", Port: 8443},
			TLS:    TLSCfg{Enabled: true, ServerName: "www.microsoft.com"},
		},
	}
	nodes = append(nodes, overrides...)
	return NodesFile{Version: KnownVersion, Token: sharedToken, Nodes: nodes}
}

func TestNodesFileTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeNodesJSON(t, dir, multiNodeFile())

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.AuthToken() != sharedToken {
		t.Fatalf("token = %q, want %q", s.AuthToken(), sharedToken)
	}
	// nodes.json wins: no node.json is ever written in multi-node mode.
	if _, err := os.Stat(filepath.Join(dir, "node.json")); !os.IsNotExist(err) {
		t.Fatal("node.json should not exist when nodes.json drives config")
	}
	if _, err := os.Stat(filepath.Join(dir, "nodes.json")); err != nil {
		t.Fatalf("nodes.json missing: %v", err)
	}
	e1, ok := s.lookupNode(1)
	if !ok || e1.Server.Port != 443 || e1.Type != "vless" {
		t.Fatalf("node 1 = %+v", e1)
	}
	e2, ok := s.lookupNode(2)
	if !ok || e2.Server.Port != 8443 || e2.Type != "hy2" {
		t.Fatalf("node 2 = %+v", e2)
	}
	if _, ok := s.lookupNode(3); ok {
		t.Fatal("node 3 must not exist")
	}
}

func TestMultiNodeAuthAndPerNodeConfig(t *testing.T) {
	dir := t.TempDir()
	writeNodesJSON(t, dir, multiNodeFile())
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := newAPIEnv(t, s)

	// Both nodes authenticate with the shared token and their own node_id.
	resp, body := e.get(t, "/api/v1/server/UniProxy/config", sharedToken, "1", "vless")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node 1 config status = %d", resp.StatusCode)
	}
	var c1 map[string]any
	if err := json.Unmarshal(body, &c1); err != nil {
		t.Fatal(err)
	}
	if c1["protocol"] != "vless" || int(c1["server_port"].(float64)) != 443 {
		t.Fatalf("node 1 config = %+v", c1)
	}

	resp, body = e.get(t, "/api/v1/server/UniProxy/config", sharedToken, "2", "hysteria2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node 2 config status = %d", resp.StatusCode)
	}
	var c2 map[string]any
	if err := json.Unmarshal(body, &c2); err != nil {
		t.Fatal(err)
	}
	if c2["protocol"] != "hysteria" || int(c2["server_port"].(float64)) != 8443 {
		t.Fatalf("node 2 config = %+v", c2)
	}

	// Wrong type for that node is rejected; a valid type for the other node is fine.
	resp, _ = e.get(t, "/api/v1/server/UniProxy/config", sharedToken, "1", "hysteria2")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("type mismatch status = %d, want 404", resp.StatusCode)
	}
	resp, _ = e.get(t, "/api/v1/server/UniProxy/config", sharedToken, "2", "vless")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("type mismatch status = %d, want 404", resp.StatusCode)
	}

	// Unknown node / wrong token.
	resp, _ = e.get(t, "/api/v1/server/UniProxy/config", sharedToken, "3", "vless")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown node status = %d, want 404", resp.StatusCode)
	}
	resp, _ = e.get(t, "/api/v1/server/UniProxy/config", "bogus", "1", "vless")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", resp.StatusCode)
	}
}

func TestPerNodeTokenOverride(t *testing.T) {
	nf := multiNodeFile()
	nf.Nodes[1].Token = "NODE2-SECRET" // node 2 has its own token
	dir := t.TempDir()
	writeNodesJSON(t, dir, nf)
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := newAPIEnv(t, s)

	// Node 2 ignores the shared token, accepts its own.
	resp, _ := e.get(t, "/api/v1/server/UniProxy/config", sharedToken, "2", "hysteria2")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("node 2 shared token status = %d, want 401", resp.StatusCode)
	}
	resp, _ = e.get(t, "/api/v1/server/UniProxy/config", "NODE2-SECRET", "2", "hysteria2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node 2 own token status = %d, want 200", resp.StatusCode)
	}
	// Node 1 still uses the shared token.
	resp, _ = e.get(t, "/api/v1/server/UniProxy/config", sharedToken, "1", "vless")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("node 1 shared token status = %d, want 200", resp.StatusCode)
	}
}

func TestDisabledNodeRejected(t *testing.T) {
	nf := multiNodeFile()
	nf.Nodes[1].Enabled = false
	dir := t.TempDir()
	writeNodesJSON(t, dir, nf)
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := newAPIEnv(t, s)
	resp, _ := e.get(t, "/api/v1/server/UniProxy/config", sharedToken, "2", "hysteria2")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled node status = %d, want 404", resp.StatusCode)
	}
}

func TestMultiNodeSharedTrafficAccumulation(t *testing.T) {
	dir := t.TempDir()
	writeNodesJSON(t, dir, multiNodeFile())
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustAddUser(t, s, User{ID: 1, UUID: "u1", Enabled: true})
	e := newAPIEnv(t, s)

	push := func(nodeID, nodeType, body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(sharedToken, nodeID, nodeType), bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("push node %s status = %d", nodeID, resp.StatusCode)
		}
	}
	push("1", "vless", `{"traffic":[{"uid":1,"up":100,"down":200,"created_at":1}]}`)
	push("2", "hysteria2", `{"traffic":[{"uid":1,"up":300,"down":400,"created_at":2}]}`)

	tf := s.SnapshotTraffic()
	if tf[1].Upload != 400 || tf[1].Download != 600 {
		t.Fatalf("traffic after both nodes = %+v, want up 400 down 600", tf[1])
	}

	// Both nodes can pull users.
	for _, tc := range []struct{ id, typ string }{{"1", "vless"}, {"2", "hysteria2"}} {
		resp, _ := e.get(t, "/api/v1/server/UniProxy/user", sharedToken, tc.id, tc.typ)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("user node %s status = %d", tc.id, resp.StatusCode)
		}
	}
}

func TestMultiNodeSaveKeepsFormat(t *testing.T) {
	dir := t.TempDir()
	writeNodesJSON(t, dir, multiNodeFile())
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveNode(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "nodes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var nf NodesFile
	if err := json.Unmarshal(data, &nf); err != nil {
		t.Fatalf("nodes.json no longer parseable: %v", err)
	}
	if nf.Token != sharedToken || len(nf.Nodes) != 2 {
		t.Fatalf("reloaded nodes.json = %+v", nf)
	}
	if _, err := os.Stat(filepath.Join(dir, "node.json")); !os.IsNotExist(err) {
		t.Fatal("SaveNode must keep writing nodes.json in multi-node mode")
	}

	// node.json must round-trip a single-node store.
	dir2 := t.TempDir()
	s2, err := NewStore(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.SaveNode(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "node.json")); err != nil {
		t.Fatalf("single-node SaveNode must write node.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "nodes.json")); !os.IsNotExist(err) {
		t.Fatal("single-node mode must not create nodes.json")
	}
}
