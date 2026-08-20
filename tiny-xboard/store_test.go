package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, dir
}

func mustAddUser(t *testing.T, s *Store, u User) {
	t.Helper()
	if err := s.AddUser(u); err != nil {
		t.Fatalf("AddUser(%d): %v", u.ID, err)
	}
}

func TestNewStoreCreatesDefaultState(t *testing.T) {
	s, dir := newTestStore(t)
	if s.NodeInfo().ID != 1 || s.NormalizedNodeType() != "vless" {
		t.Fatalf("unexpected default node: %+v", s.NodeInfo())
	}
	if s.AuthToken() == "" {
		t.Fatal("token should not be empty")
	}
	for _, f := range []string{"node.json", "users.json", "traffic.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
}

func TestCorruptJSONRecoveryFromBackup(t *testing.T) {
	dir := t.TempDir()

	valid := `{"version":1,"users":[{"id":7,"uuid":"u-7","name":"alice","speed_limit":3,"enabled":true}]}`
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(`{corrupt`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.json.bak"), []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore should recover from backup: %v", err)
	}
	users := s.EnabledUsers()
	if len(users) != 1 || users[0].ID != 7 {
		t.Fatalf("users = %+v", users)
	}

	// The main file should have been restored as valid JSON.
	var uf UsersFile
	raw, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &uf); err != nil {
		t.Fatalf("restored main file is still invalid JSON: %v", err)
	}
}

func TestCorruptJSONBothFail(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.json"), []byte(`{corrupt`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.json.bak"), []byte(`{also corrupt`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir); err == nil {
		t.Fatal("expected error when node.json and backup are both invalid")
	}
}

func TestTrafficAccumulationAndUnknownIgnored(t *testing.T) {
	s, _ := newTestStore(t)
	mustAddUser(t, s, User{ID: 1, UUID: "u", Enabled: true})

	if !s.Accumulate(1, 100, 50) {
		t.Fatal("known user should be accepted")
	}
	if !s.Accumulate(1, 25, 25) {
		t.Fatal("second accumulate should be accepted")
	}
	if s.Accumulate(999, 1, 1) {
		t.Fatal("unknown user must be rejected")
	}
	if s.Accumulate(1, -5, 0) {
		t.Fatal("negative upload must be rejected")
	}

	tf := s.SnapshotTraffic()
	if len(tf) != 1 {
		t.Fatalf("traffic entries = %d, want 1 (unknown user must not pollute)", len(tf))
	}
	if tf[1].Upload != 125 || tf[1].Download != 75 {
		t.Fatalf("traffic = %+v, want {125,75}", tf[1])
	}
}

func TestMaxUsersEnforced(t *testing.T) {
	s, _ := newTestStore(t)
	tooMany := make([]User, MaxUsers+1)
	for i := range tooMany {
		tooMany[i] = User{ID: i + 1, UUID: "u"}
	}
	if err := s.SetUsers(tooMany); err == nil {
		t.Fatal("expected error when exceeding MaxUsers")
	}
}

func TestConcurrentUserWrites(t *testing.T) {
	s, dir := newTestStore(t)
	const n = 24

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := i + 1
			errs[i] = s.AddUser(User{ID: id, UUID: "u", Name: "user-" + strconv.Itoa(id), Enabled: true})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("AddUser %d: %v", i+1, err)
		}
	}

	users := s.EnabledUsers()
	if len(users) != n {
		t.Fatalf("len(users) = %d, want %d", len(users), n)
	}

	// Reload the file from disk: every user must be persisted.
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.EnabledUsers()); got != n {
		t.Fatalf("reloaded users = %d, want %d", got, n)
	}
}

func TestSetUsersPrunesTraffic(t *testing.T) {
	s, _ := newTestStore(t)
	mustAddUser(t, s, User{ID: 1, UUID: "u1"})
	mustAddUser(t, s, User{ID: 2, UUID: "u2"})
	if !s.Accumulate(1, 10, 10) || !s.Accumulate(2, 20, 20) {
		t.Fatal("accumulate failed")
	}

	// Removing user 2 must also drop its traffic.
	if err := s.SetUsers([]User{{ID: 1, UUID: "u1"}}); err != nil {
		t.Fatal(err)
	}
	tf := s.SnapshotTraffic()
	if len(tf) != 1 || tf[1].Upload != 10 {
		t.Fatalf("traffic after prune = %+v", tf)
	}
}

func TestRefreshUsersReloadsEditedFile(t *testing.T) {
	s, dir := newTestStore(t)
	if got := len(s.EnabledUsers()); got != 0 {
		t.Fatalf("initial users = %d, want 0", got)
	}

	// Write users.json directly (as an operator would) and refresh.
	edited := `{"version":1,"users":[{"id":5,"uuid":"u5","name":"carol","enabled":true}]}`
	path := filepath.Join(dir, "users.json")
	if err := os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	// mtime is 1s-granular on some filesystems; ensure the edit is visible.
	if err := os.Chtimes(path, time.Now(), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshUsers(); err != nil {
		t.Fatalf("RefreshUsers: %v", err)
	}
	users := s.EnabledUsers()
	if len(users) != 1 || users[0].ID != 5 || users[0].Name != "carol" {
		t.Fatalf("reloaded users = %+v", users)
	}

	// Second refresh with no change must be a no-op (idempotent).
	if err := s.RefreshUsers(); err != nil {
		t.Fatalf("RefreshUsers (noop): %v", err)
	}
	if len(s.EnabledUsers()) != 1 {
		t.Fatalf("users changed unexpectedly: %+v", s.EnabledUsers())
	}

	// Removing a user via file edit must prune its traffic.
	if !s.Accumulate(5, 100, 100) {
		t.Fatal("accumulate failed")
	}
	edited = `{"version":1,"users":[]}`
	if err := os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), time.Now().Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshUsers(); err != nil {
		t.Fatalf("RefreshUsers (prune): %v", err)
	}
	if got := len(s.EnabledUsers()); got != 0 {
		t.Fatalf("users after prune = %d", got)
	}
	if tf := s.SnapshotTraffic(); len(tf) != 0 {
		t.Fatalf("traffic after prune = %+v", tf)
	}
}
