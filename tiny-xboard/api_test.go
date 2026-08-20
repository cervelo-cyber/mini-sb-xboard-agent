package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type apiEnv struct {
	srv   *httptest.Server
	store *Store
	token string
}

func newAPIEnv(t *testing.T, s *Store) *apiEnv {
	t.Helper()
	if s == nil {
		s, _ = newTestStore(t)
	}
	return &apiEnv{
		srv:   httptest.NewServer((&Server{store: s}).routes()),
		store: s,
		token: s.AuthToken(),
	}
}

func (e *apiEnv) get(t *testing.T, path string, token, nodeID, nodeType string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+path+query(token, nodeID, nodeType), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func query(token, nodeID, nodeType string) string {
	q := ""
	if token != "" {
		q += "token=" + token + "&"
	}
	if nodeID != "" {
		q += "node_id=" + nodeID + "&"
	}
	if nodeType != "" {
		q += "node_type=" + nodeType + "&"
	}
	if q == "" {
		return ""
	}
	return "?" + q[:len(q)-1]
}

func TestTokenAuthentication(t *testing.T) {
	e := newAPIEnv(t, nil)

	cases := []struct {
		name     string
		token    string
		nodeID   string
		nodeType string
		want     int
	}{
		{"no token", "", "1", "vless", http.StatusUnauthorized},
		{"wrong token", "nope", "1", "vless", http.StatusUnauthorized},
		{"right token wrong node", e.token, "99", "vless", http.StatusNotFound},
		{"right everything", e.token, "1", "vless", http.StatusOK},
		{"wrong node type", e.token, "1", "hysteria2", http.StatusNotFound},
	}
	for _, tc := range cases {
		resp, _ := e.get(t, "/api/v1/server/UniProxy/user", tc.token, tc.nodeID, tc.nodeType)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

func TestHandleUserReturnsEnabledUsersSorted(t *testing.T) {
	s, _ := newTestStore(t)
	for _, u := range []User{
		{ID: 3, UUID: "u3", Name: "c", Enabled: true, SpeedLimit: 5},
		{ID: 1, UUID: "u1", Name: "a", Enabled: false},
		{ID: 2, UUID: "u2", Enabled: true},
	} {
		if err := s.AddUser(u); err != nil {
			t.Fatal(err)
		}
	}
	e := newAPIEnv(t, s)

	resp, body := e.get(t, "/api/v1/server/UniProxy/user", e.token, "1", "vless")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Users []User `json:"users"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Users) != 2 || out.Users[0].ID != 2 || out.Users[1].ID != 3 {
		t.Fatalf("users = %+v, want [2 3]", out.Users)
	}
	// Disabled user (id 1) must be filtered out.
	for _, u := range out.Users {
		if u.ID == 1 {
			t.Fatal("disabled user must not be returned")
		}
	}
}

func TestHandleConfigVless(t *testing.T) {
	e := newAPIEnv(t, nil)

	resp, body := e.get(t, "/api/v1/server/UniProxy/config", e.token, "1", "vless")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["protocol"] != "vless" {
		t.Fatalf("protocol = %v", cfg["protocol"])
	}
	if cfg["tls"].(float64) != 2 {
		t.Fatalf("tls = %v, want 2 (reality)", cfg["tls"])
	}
	base := cfg["base_config"].(map[string]any)
	if base["push_interval"].(float64) != DefaultSyncInterval {
		t.Fatalf("base_config = %v", base)
	}
}

func TestHandleConfigHysteria2(t *testing.T) {
	// A node.json describing an hysteria2 node; probe should match hysteria node_type.
	dir := t.TempDir()
	nodeJSON := `{
	  "version": 1,
	  "node": {"id": 5, "name": "hy2-01", "type": "hysteria2", "enabled": true},
	  "auth": {"token": "hy2token"},
	  "server": {"listen": "0.0.0.0", "port": 44311},
	  "protocol": {"type": "hysteria2"},
	  "tls": {"enabled": true, "server_name": "www.apple.com", "allow_insecure": true},
	  "runtime": {"sync_interval": 60, "traffic_flush_interval": 300}
	}`
	if err := os.WriteFile(filepath.Join(dir, "node.json"), []byte(nodeJSON), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := newAPIEnv(t, s)

	resp, body := e.get(t, "/api/v1/server/UniProxy/config", "hy2token", "5", "hysteria")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["protocol"] != "hysteria" || cfg["version"].(float64) != 2 {
		t.Fatalf("protocol/version = %v/%v", cfg["protocol"], cfg["version"])
	}
	if cfg["server_port"].(float64) != 44311 {
		t.Fatalf("server_port = %v", cfg["server_port"])
	}
	if cfg["tls_settings"].(map[string]any)["server_name"] != "www.apple.com" {
		t.Fatalf("tls_settings = %v", cfg["tls_settings"])
	}
}

func TestPushAccumulatesAndIgnoresUnknown(t *testing.T) {
	s, _ := newTestStore(t)
	mustAddUser(t, s, User{ID: 1, UUID: "u1"})
	mustAddUser(t, s, User{ID: 2, UUID: "u2"})
	e := newAPIEnv(t, s)

	payload := `{"1":[100,50],"2":[200,300],"999":[1,1],"":[]}`
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"), bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	tf := s.SnapshotTraffic()
	if len(tf) != 2 {
		t.Fatalf("traffic entries = %d, want 2", len(tf))
	}
	if tf[1].Upload != 100 || tf[1].Download != 50 || tf[2].Upload != 200 || tf[2].Download != 300 {
		t.Fatalf("traffic = %+v", tf)
	}
}

func TestPushTrafficArrayFormat(t *testing.T) {
	s, _ := newTestStore(t)
	mustAddUser(t, s, User{ID: 1, UUID: "u1"})
	mustAddUser(t, s, User{ID: 2, UUID: "u2"})
	e := newAPIEnv(t, s)

	// Real Xboard UniProxy format: {"traffic":[{uid,up,down,created_at}]}
	payload := `{"traffic":[{"uid":1,"up":100,"down":50,"created_at":1712000000},{"uid":2,"up":200,"down":300,"created_at":1712000000},{"uid":999,"up":1,"down":1,"created_at":1712000000}]}`
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"), bytes.NewBufferString(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var out struct {
		Accepted int `json:"accepted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Accepted != 2 {
		t.Fatalf("accepted = %d, want 2 (unknown uid 999 must be ignored)", out.Accepted)
	}

	tf := s.SnapshotTraffic()
	if len(tf) != 2 {
		t.Fatalf("traffic entries = %d, want 2", len(tf))
	}
	if tf[1].Upload != 100 || tf[1].Download != 50 || tf[2].Upload != 200 || tf[2].Download != 300 {
		t.Fatalf("traffic = %+v", tf)
	}

	// Malformed traffic array must be a 400.
	badReq, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"), bytes.NewBufferString(`{"traffic":"nope"}`))
	badReq.Header.Set("Content-Type", "application/json")
	badResp, err := e.srv.Client().Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed traffic status = %d, want 400", badResp.StatusCode)
	}
}

func TestPushMethodNotAllowed(t *testing.T) {
	e := newAPIEnv(t, nil)
	resp, body := e.get(t, "/api/v1/server/UniProxy/push", e.token, "1", "vless")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
}

func TestConcurrentTrafficPush(t *testing.T) {
	s, dir := newTestStore(t)
	const users = 10
	for i := 1; i <= users; i++ {
		mustAddUser(t, s, User{ID: i, UUID: "u" + strconv.Itoa(i)})
	}
	e := newAPIEnv(t, s)

	const goroutines = 24
	const iterations = 8
	expected := make(map[int][2]int64)
	var expectedMu sync.Mutex

	var wg sync.WaitGroup
	var accepted atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				body := map[string][]int64{}
				for u := 1; u <= users; u++ {
					up := int64(100*u + g)
					down := int64(50*u + it)
					body[strconv.Itoa(u)] = []int64{up, down}
					expectedMu.Lock()
					exp := expected[u]
					exp[0] += up
					exp[1] += down
					expected[u] = exp
					expectedMu.Unlock()
				}
				body["424242"] = []int64{1, 1} // unknown, must be ignored
				raw, _ := json.Marshal(body)
				req, err := http.NewRequest(http.MethodPost,
					e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"),
					bytes.NewBuffer(raw))
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := e.srv.Client().Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("status = %d", resp.StatusCode)
					return
				}
				accepted.Add(1)
			}
		}(g)
	}
	wg.Wait()

	// Flush and read back the persisted totals.
	s.flushTrafficIfDirty()
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tf := reloaded.SnapshotTraffic()

	expectedMu.Lock()
	defer expectedMu.Unlock()
	if len(tf) != users {
		t.Fatalf("traffic entries = %d, want %d (unknown ids must not leak)", len(tf), users)
	}
	for u := 1; u <= users; u++ {
		if tf[u].Upload != expected[u][0] || tf[u].Download != expected[u][1] {
			t.Fatalf("user %d traffic = %+v, want %v", u, tf[u], expected[u])
		}
	}
	if accepted.Load() != goroutines*iterations {
		t.Fatalf("accepted = %d, want %d", accepted.Load(), goroutines*iterations)
	}
}

func TestGracefulShutdownFlushesTraffic(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustAddUser(t, s, User{ID: 1, UUID: "u1"})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, ln) }()

	// Wait until the server accepts requests.
	addr := "http://" + ln.Addr().String()
	waitForReady(t, addr, s.AuthToken())

	// Push some traffic, then initiate a graceful shutdown.
	if _, err := doPost(t, addr+"/api/v1/server/UniProxy/push"+query(s.AuthToken(), "1", "vless"), `{"1":[123,456]}`); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}

	// Traffic must have been flushed to disk before exit.
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tf := reloaded.SnapshotTraffic()
	if tf[1].Upload != 123 || tf[1].Download != 456 {
		t.Fatalf("traffic after graceful shutdown = %+v, want {123,456}", tf[1])
	}
}

func waitForReady(t *testing.T, base, token string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := doPost(t, base+"/api/v1/server/UniProxy/push"+query(token, "1", "vless"), `{}`); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not become ready")
}

func doPost(t *testing.T, url, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}
