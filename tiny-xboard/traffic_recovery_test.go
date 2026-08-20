package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

const validNodeJSON = `{
  "version": 1,
  "node": {"id": 1, "name": "n1", "type": "vless", "enabled": true},
  "auth": {"token": "T"},
  "server": {"listen": "0.0.0.0", "port": 443},
  "tls": {"enabled": true, "server_name": "www.microsoft.com", "server_port": 443},
  "runtime": {"sync_interval": 60, "traffic_flush_interval": 300}
}`

func writeStateFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// newTrafficStore builds a store whose node/users files are fixed and whose
// traffic.json (.bak) is exactly as given. A non-nil users slice populates
// users.json so traffic counters for those ids survive pruneTraffic.
func newTrafficStore(t *testing.T, trafficContent, trafficBak string, users ...User) *Store {
	t.Helper()
	dir := t.TempDir()
	writeStateFile(t, dir, "node.json", validNodeJSON)
	if trafficContent != "" {
		writeStateFile(t, dir, "traffic.json", trafficContent)
	}
	if trafficBak != "" {
		writeStateFile(t, dir, "traffic.json.bak", trafficBak)
	}
	if users == nil {
		users = []User{}
	}
	raw, _ := json.Marshal(UsersFile{Version: 1, Users: users})
	writeStateFile(t, dir, "users.json", string(raw))
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore must never fail because of traffic: %v", err)
	}
	return s
}

// G1 matrix 1 & 6: traffic.json (and bak) missing -> server starts, empty.
func TestTrafficMissingFileStartsServer(t *testing.T) {
	s := newTrafficStore(t, "", "")
	if tf := s.SnapshotTraffic(); len(tf) != 0 {
		t.Fatalf("traffic = %+v, want empty", tf)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "traffic.json")); err != nil {
		t.Fatalf("expected empty traffic.json to be created: %v", err)
	}
}

// G1 matrix 2: valid traffic.json -> counters loaded.
func TestTrafficValidFileLoaded(t *testing.T) {
	s := newTrafficStore(t,
		`{"version":1,"updated_at":1,"users":{"1":{"upload":100,"download":50}}}`,
		"", User{ID: 1, UUID: "u1"})
	tf := s.SnapshotTraffic()
	if len(tf) != 1 || tf[1].Upload != 100 || tf[1].Download != 50 {
		t.Fatalf("traffic = %+v, want user 1 = {100,50}", tf)
	}
}

// G1 matrix 3: invalid JSON -> server starts, empty traffic.
func TestTrafficInvalidJSONStartsWithEmpty(t *testing.T) {
	s := newTrafficStore(t, `{corrupt`, "")
	if tf := s.SnapshotTraffic(); len(tf) != 0 {
		t.Fatalf("traffic = %+v, want empty", tf)
	}
}

// G1 matrix 4: corrupt main + valid bak -> bak loaded.
func TestTrafficRecoversFromBackup(t *testing.T) {
	s := newTrafficStore(t, `{corrupt`,
		`{"version":1,"users":{"1":{"upload":7,"download":8}}}`, User{ID: 1, UUID: "u1"})
	tf := s.SnapshotTraffic()
	if len(tf) != 1 || tf[1].Upload != 7 || tf[1].Download != 8 {
		t.Fatalf("traffic = %+v, want user 1 = {7,8} from backup", tf)
	}
	// The main file must have been restored as valid JSON.
	var tf2 TrafficFile
	raw, err := os.ReadFile(filepath.Join(s.dir, "traffic.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &tf2); err != nil {
		t.Fatalf("restored main file is still invalid: %v", err)
	}
}

// G1 matrix 5: main + bak both corrupt -> server starts, empty traffic.
func TestTrafficBothCorruptStartsWithEmpty(t *testing.T) {
	s := newTrafficStore(t, `{corrupt`, `{also bad`)
	if tf := s.SnapshotTraffic(); len(tf) != 0 {
		t.Fatalf("traffic = %+v, want empty", tf)
	}
}

// G1 matrix 7: negative upload clamped to 0.
func TestTrafficNegativeUploadClamped(t *testing.T) {
	s := newTrafficStore(t, `{"version":1,"users":{"1":{"upload":-5,"download":0}}}`, "", User{ID: 1, UUID: "u1"})
	tf := s.SnapshotTraffic()
	if len(tf) != 1 || tf[1].Upload != 0 || tf[1].Download != 0 {
		t.Fatalf("traffic = %+v, want upload clamped to 0", tf)
	}
}

// G1 matrix 8: negative download clamped to 0.
func TestTrafficNegativeDownloadClamped(t *testing.T) {
	s := newTrafficStore(t, `{"version":1,"users":{"1":{"upload":10,"download":-5}}}`, "", User{ID: 1, UUID: "u1"})
	tf := s.SnapshotTraffic()
	if len(tf) != 1 || tf[1].Upload != 10 || tf[1].Download != 0 {
		t.Fatalf("traffic = %+v, want download clamped to 0", tf)
	}
}

// G1 matrix 9: unknown user ids are dropped, not fatal.
func TestTrafficUnknownUserIgnored(t *testing.T) {
	s := newTrafficStore(t, `{"version":1,"users":{"999":{"upload":1,"download":1}}}`, "")
	if tf := s.SnapshotTraffic(); len(tf) != 0 {
		t.Fatalf("traffic = %+v, want unknown user pruned", tf)
	}
}

// G1 matrix 10: an oversized traffic file must be rejected by the size cap, not
// blow up memory, and must not prevent startup.
func TestTrafficOversizedFileDoesNotOOM(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, "node.json", validNodeJSON)
	big := bytes.Repeat([]byte{'x'}, MaxStateFileSize+1)
	writeStateFile(t, dir, "traffic.json", string(big))
	writeStateFile(t, dir, "users.json", `{"version":1,"users":[]}`)
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("oversized traffic must not prevent startup: %v", err)
	}
	if tf := s.SnapshotTraffic(); len(tf) != 0 {
		t.Fatalf("traffic = %+v, want empty", tf)
	}
}

// G1 matrix 11-13: with corrupt traffic, /user, /config and /push all keep
// working and traffic still accumulates in memory.
func TestTrafficCorruptAPIsStillWork(t *testing.T) {
	s := newTrafficStore(t, `{corrupt`, "", User{ID: 1, UUID: "u1"})
	e := newAPIEnv(t, s)

	resp, _ := e.get(t, "/api/v1/server/UniProxy/user", e.token, "1", "vless")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/user status = %d", resp.StatusCode)
	}
	resp, body := e.get(t, "/api/v1/server/UniProxy/config", e.token, "1", "vless")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/config status = %d", resp.StatusCode)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil || cfg["protocol"] != "vless" {
		t.Fatalf("/config body = %s err=%v", body, err)
	}

	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/server/UniProxy/push"+query(e.token, "1", "vless"), bytes.NewBufferString(`{"1":[5,6]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/push status = %d", resp.StatusCode)
	}
	tf := s.SnapshotTraffic()
	if len(tf) != 1 || tf[1].Upload != 5 || tf[1].Download != 6 {
		t.Fatalf("traffic after push = %+v, want {5,6}", tf)
	}
}

// G1 matrix 8b: traffic flush failure (read-only dir) must not crash the
// process; the flush path logs and retries (RAM kept).
func TestTrafficFlushFailureIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir, "node.json", validNodeJSON)
	writeStateFile(t, dir, "users.json", `{"version":1,"users":[{"id":1,"uuid":"u1"}]}`)
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Accumulate(1, 9, 9) {
		t.Fatal("accumulate failed")
	}
	// Break the data dir so SaveTraffic cannot write.
	os.Chmod(dir, 0500)
	defer os.Chmod(dir, 0700)
	s.dirty.Store(true)
	s.flushTrafficIfDirty() // must not panic / crash
	// RAM must still hold the counters.
	tf := s.SnapshotTraffic()
	if len(tf) != 1 || tf[1].Upload != 9 {
		t.Fatalf("traffic after failed flush = %+v (RAM must be kept)", tf)
	}
}
