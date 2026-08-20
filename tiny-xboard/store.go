package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// KnownVersion is the current on-disk schema version for all state files.
const KnownVersion = 1

// MaxUsers caps the number of users kept in RAM and on disk so state can never
// grow without bound.
const MaxUsers = 1000

// Default traffic flush interval (seconds) and panel sync interval (seconds).
const (
	DefaultSyncInterval         = 60
	DefaultTrafficFlushInterval = 300
)

// ---------------------------------------------------------------------------
// node.json schema
// ---------------------------------------------------------------------------

type NodeFile struct {
	Version  int         `json:"version"`
	Node     NodeInfo    `json:"node"`
	Auth     AuthCfg     `json:"auth"`
	Server   ServerCfg   `json:"server"`
	Protocol ProtocolCfg `json:"protocol"`
	TLS      TLSCfg      `json:"tls"`
	Runtime  RuntimeCfg  `json:"runtime"`
}

// NodesFile is the multi-node schema. When nodes.json exists it takes
// precedence over node.json: all nodes share the panel token (通讯密钥) and are
// distinguished by node id. An entry may override the token per-node.
type NodesFile struct {
	Version int         `json:"version"`
	Token   string      `json:"token"`
	Nodes   []NodeEntry `json:"nodes"`
}

// NodeEntry is one node managed by a tiny-xboard instance.
type NodeEntry struct {
	ID       int         `json:"id"`
	Name     string      `json:"name,omitempty"`
	Type     string      `json:"type"`
	Enabled  bool        `json:"enabled"`
	Token    string      `json:"token,omitempty"` // optional per-node token override
	Server   ServerCfg   `json:"server"`
	Protocol ProtocolCfg `json:"protocol"`
	TLS      TLSCfg      `json:"tls"`
	Runtime  RuntimeCfg  `json:"runtime"`
}

func entryFromNodeFile(nf NodeFile) NodeEntry {
	return NodeEntry{
		ID:       nf.Node.ID,
		Name:     nf.Node.Name,
		Type:     nf.Node.Type,
		Enabled:  nf.Node.Enabled,
		Server:   nf.Server,
		Protocol: nf.Protocol,
		TLS:      nf.TLS,
		Runtime:  nf.Runtime,
	}
}

func nodeFileFromEntry(e NodeEntry, token string) NodeFile {
	return NodeFile{
		Version: KnownVersion,
		Node:    NodeInfo{ID: e.ID, Name: e.Name, Type: e.Type, Enabled: e.Enabled},
		Auth:    AuthCfg{Token: token},
		Server:  e.Server, Protocol: e.Protocol, TLS: e.TLS, Runtime: e.Runtime,
	}
}

type NodeInfo struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
}

type AuthCfg struct {
	Token string `json:"token"`
}

type ServerCfg struct {
	Listen string `json:"listen"`
	Port   int    `json:"port"`
}

type ProtocolCfg struct {
	Type       string `json:"type"`
	Network    string `json:"network"`
	Flow       string `json:"flow"`
	Decryption string `json:"decryption"`
}

type TLSCfg struct {
	Enabled       bool   `json:"enabled"`
	ServerName    string `json:"server_name"`
	ServerPort    int    `json:"server_port"`
	PublicKey     string `json:"public_key"`
	PrivateKey    string `json:"private_key"`
	ShortID       string `json:"short_id"`
	AllowInsecure bool   `json:"allow_insecure"`
}

type RuntimeCfg struct {
	SyncInterval         int `json:"sync_interval"`
	TrafficFlushInterval int `json:"traffic_flush_interval"`
}

// ---------------------------------------------------------------------------
// users.json schema
// ---------------------------------------------------------------------------

type UsersFile struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
}

type User struct {
	ID         int    `json:"id"`
	UUID       string `json:"uuid"`
	Password   string `json:"password,omitempty"`
	Name       string `json:"name,omitempty"`
	SpeedLimit int    `json:"speed_limit,omitempty"`
	Enabled    bool   `json:"enabled"`
}

// ---------------------------------------------------------------------------
// traffic.json schema
// ---------------------------------------------------------------------------

type TrafficFile struct {
	Version   int                `json:"version"`
	UpdatedAt int64              `json:"updated_at"`
	Users     map[string]Traffic `json:"users"`
}

type Traffic struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type Store struct {
	dir string

	// mu guards the in-memory state below.
	mu     sync.RWMutex
	saveMu sync.Mutex // serializes disk writes for all three files

	nodeList   []NodeEntry // one or more managed nodes
	panelToken string      // shared 通讯密钥 (single-node mode: node.json auth.token)
	nodesMulti bool        // true when nodes.json drives configuration
	users      map[int]User
	traffic    map[int]*Traffic

	// usersMod/usersLoaded track the users.json state so RefreshUsers can
	// reload only when the file actually changed (guarded by mu).
	usersMod    time.Time
	usersLoaded bool

	// dirty marks that cumulative traffic changed since the last flush.
	dirty atomic.Bool
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return nil, err
	}
	s := &Store{
		dir:     dir,
		users:   make(map[int]User),
		traffic: make(map[int]*Traffic),
	}
	if err := s.loadNode(); err != nil {
		return nil, fmt.Errorf("load node: %w", err)
	}
	if err := s.loadUsers(); err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	s.loadTraffic()
	s.pruneTraffic()
	return s, nil
}

func (s *Store) nodePath() string    { return filepath.Join(s.dir, "node.json") }
func (s *Store) nodesPath() string   { return filepath.Join(s.dir, "nodes.json") }
func (s *Store) usersPath() string   { return filepath.Join(s.dir, "users.json") }
func (s *Store) trafficPath() string { return filepath.Join(s.dir, "traffic.json") }

// ---------------------------------------------------------------------------
// loading
// ---------------------------------------------------------------------------

// loadNode prefers nodes.json (multi-node) and falls back to the legacy
// single-node node.json.
func (s *Store) loadNode() error {
	if _, err := os.Stat(s.nodesPath()); err == nil {
		return s.loadNodesFile()
	}
	return s.loadSingleNode()
}

func (s *Store) loadSingleNode() error {
	path := s.nodePath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		nf := defaultNode()
		entry := entryFromNodeFile(nf)
		if _, err := ensureRealityKeypair(&entry); err != nil {
			return err
		}
		s.mu.Lock()
		s.nodeList = []NodeEntry{entry}
		s.panelToken = nf.Auth.Token
		s.nodesMulti = false
		s.mu.Unlock()
		if err := s.SaveNode(); err != nil {
			return err
		}
		log.Printf("[warn] %s did not exist; created with a new random token", path)
		return nil
	}

	var nf NodeFile
	usedBak, err := loadWithFallback(path, &nf)
	if err != nil {
		return err
	}
	if err := validateNode(nf); err != nil {
		// Main file decoded but failed semantic validation: retry from backup.
		var firstErr = err
		var nf2 NodeFile
		if err := LoadFile(path+".bak", &nf2); err != nil {
			return fmt.Errorf("node.json invalid and backup unusable: %v", firstErr)
		}
		if err := validateNode(nf2); err != nil {
			return fmt.Errorf("node.json invalid and backup invalid: %v", firstErr)
		}
		nf, usedBak = nf2, true
	}
	entry := entryFromNodeFile(nf)
	changed, err := ensureRealityKeypair(&entry)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.nodeList = []NodeEntry{entry}
	s.panelToken = nf.Auth.Token
	s.nodesMulti = false
	s.mu.Unlock()
	if usedBak || changed {
		if err := s.SaveNode(); err != nil { // restore the main file / persist the keypair
			return err
		}
		if usedBak {
			log.Printf("[warn] recovered %s from backup", path)
		}
		if changed {
			log.Printf("[warn] node %d: generated a new VLESS Reality keypair (persisted; reused on later starts)", entry.ID)
		}
	}
	return nil
}

func (s *Store) loadNodesFile() error {
	path := s.nodesPath()
	var nf NodesFile
	usedBak, err := loadWithFallback(path, &nf)
	if err != nil {
		return err
	}
	if err := validateNodesFile(nf); err != nil {
		var firstErr = err
		var nf2 NodesFile
		if err := LoadFile(path+".bak", &nf2); err != nil {
			return fmt.Errorf("nodes.json invalid and backup unusable: %v", firstErr)
		}
		if err := validateNodesFile(nf2); err != nil {
			return fmt.Errorf("nodes.json invalid and backup invalid: %v", firstErr)
		}
		nf, usedBak = nf2, true
	}
	nodes := append([]NodeEntry(nil), nf.Nodes...)
	changed := false
	for i := range nodes {
		c, err := ensureRealityKeypair(&nodes[i])
		if err != nil {
			return err
		}
		changed = changed || c
	}
	s.mu.Lock()
	s.nodeList = nodes
	s.panelToken = nf.Token
	s.nodesMulti = true
	s.mu.Unlock()
	if usedBak || changed {
		if err := s.SaveNode(); err != nil { // restore the main file / persist keypairs
			return err
		}
		if usedBak {
			log.Printf("[warn] recovered %s from backup", path)
		}
		if changed {
			log.Printf("[warn] generated new VLESS Reality keypair(s) for node(s) in %s (persisted; reused on later starts)", path)
		}
	}
	return nil
}

// loadUsersFromDisk reads and validates users.json (falling back to the
// backup). It performs file IO only and takes no locks. A missing file yields
// an empty file with no error.
func loadUsersFromDisk(path string) (UsersFile, bool, error) {
	var uf UsersFile
	usedBak, err := loadWithFallback(path, &uf)
	if err != nil {
		if os.IsNotExist(err) {
			return uf, false, nil
		}
		return uf, false, err
	}
	if err := validateUsersFile(uf); err != nil {
		var uf2 UsersFile
		if err := LoadFile(path+".bak", &uf2); err != nil {
			return uf, false, fmt.Errorf("users.json invalid and backup unusable: %v", err)
		}
		if err := validateUsersFile(uf2); err != nil {
			return uf, false, fmt.Errorf("users.json invalid and backup invalid: %v", err)
		}
		uf, usedBak = uf2, true
	}
	return uf, usedBak, nil
}

func (s *Store) loadUsers() error {
	path := s.usersPath()
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s.SaveUsers()
		}
		return err
	}

	uf, usedBak, err := loadUsersFromDisk(path)
	if err != nil {
		return err
	}
	m := make(map[int]User, len(uf.Users))
	for _, u := range uf.Users {
		m[u.ID] = u
	}
	s.mu.Lock()
	s.users = m
	s.usersLoaded = true
	s.usersMod = info.ModTime()
	s.mu.Unlock()
	if usedBak {
		if err := s.SaveUsers(); err != nil {
			return err
		}
		log.Printf("[warn] recovered %s from backup", path)
	}
	return nil
}

// RefreshUsers reloads users.json if it changed since the last load, so edits
// made while the server runs take effect immediately (no restart needed). It
// prunes traffic counters of removed users. Errors are returned but are never
// fatal; the in-memory set is kept on failure.
func (s *Store) RefreshUsers() error {
	path := s.usersPath()
	info, err := os.Stat(path)
	if err != nil {
		return nil // file missing or unreadable: keep the current set
	}
	s.mu.RLock()
	changed := !s.usersLoaded || info.ModTime().After(s.usersMod)
	s.mu.RUnlock()
	if !changed {
		return nil
	}

	uf, usedBak, err := loadUsersFromDisk(path)
	if err != nil {
		return err
	}
	m := make(map[int]User, len(uf.Users))
	for _, u := range uf.Users {
		m[u.ID] = u
	}
	s.mu.Lock()
	s.users = m
	s.usersLoaded = true
	s.usersMod = info.ModTime()
	pruned := false
	for id := range s.traffic {
		if _, ok := m[id]; !ok {
			delete(s.traffic, id)
			pruned = true
		}
	}
	s.mu.Unlock()
	if pruned {
		s.dirty.Store(true)
	}
	if usedBak {
		return s.SaveUsers()
	}
	return nil
}

// loadTraffic loads cumulative traffic stats from disk. Unlike node/users,
// traffic is disposable derived state: corruption or loss must never prevent
// the server from starting. Every failure is logged and the store falls back
// to an empty traffic map; the HTTP endpoints remain fully available. Negative
// counters are clamped to zero and unknown user ids are dropped (pruneTraffic
// removes them for good once users are known).
func (s *Store) loadTraffic() {
	path := s.trafficPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := s.SaveTraffic(); err != nil {
			log.Printf("[warn] failed to create %s: %v", path, err)
		}
		return
	}

	var tf TrafficFile
	usedBak, err := loadWithFallback(path, &tf)
	if err != nil {
		log.Printf("[warn] load traffic: %v (starting with empty traffic)", err)
		s.traffic = make(map[int]*Traffic)
		return
	}
	if verr := validateTrafficFile(tf); verr != nil {
		var tf2 TrafficFile
		if err := LoadFile(path+".bak", &tf2); err != nil {
			log.Printf("[warn] traffic.json invalid and backup unusable (%v); starting with empty traffic", verr)
			s.traffic = make(map[int]*Traffic)
			return
		}
		if verr2 := validateTrafficFile(tf2); verr2 != nil {
			log.Printf("[warn] traffic.json invalid and backup invalid (%v); starting with empty traffic", verr)
			s.traffic = make(map[int]*Traffic)
			return
		}
		tf, usedBak = tf2, true
	}
	s.traffic = normalizeTrafficFile(tf)
	if usedBak {
		if err := s.SaveTraffic(); err != nil { // restore the main file
			log.Printf("[warn] failed to restore %s from backup: %v", path, err)
		} else {
			log.Printf("[warn] recovered %s from backup", path)
		}
	}
}

// normalizeTrafficFile converts an on-disk traffic map into the in-memory form,
// clamping negative counters to zero and ignoring malformed/unknown ids.
func normalizeTrafficFile(tf TrafficFile) map[int]*Traffic {
	m := make(map[int]*Traffic, len(tf.Users))
	for idStr, t := range tf.Users {
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			continue
		}
		v := Traffic{Upload: t.Upload, Download: t.Download}
		if v.Upload < 0 {
			v.Upload = 0
		}
		if v.Download < 0 {
			v.Download = 0
		}
		m[id] = &v
	}
	return m
}

// pruneTraffic drops counters that no longer belong to a known user so the
// traffic map stays bounded by the live user list.
func (s *Store) pruneTraffic() {
	s.mu.Lock()
	for id := range s.traffic {
		if _, ok := s.users[id]; !ok {
			delete(s.traffic, id)
		}
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

func normalizeNodeType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "vless", "vless-reality", "reality":
		return "vless"
	case "hy2", "hysteria", "hysteria2":
		return "hy2"
	case "both", "dual", "all":
		return "both"
	default:
		return strings.ToLower(strings.TrimSpace(t))
	}
}

func validateNode(nf NodeFile) error {
	if nf.Version != 0 && nf.Version > KnownVersion {
		return fmt.Errorf("unsupported node.json version %d", nf.Version)
	}
	if nf.Node.ID <= 0 {
		return fmt.Errorf("node.id must be positive")
	}
	switch normalizeNodeType(nf.Node.Type) {
	case "vless", "hy2", "both":
	default:
		return fmt.Errorf("node.type must be vless, hy2 or both (got %q)", nf.Node.Type)
	}
	if nf.Auth.Token == "" {
		return fmt.Errorf("auth.token must not be empty")
	}
	if nf.Server.Port < 1 || nf.Server.Port > 65535 {
		return fmt.Errorf("server.port must be in 1..65535 (got %d)", nf.Server.Port)
	}
	return nil
}

func validateNodeEntry(n NodeEntry) error {
	if n.ID <= 0 {
		return fmt.Errorf("node.id must be positive")
	}
	switch normalizeNodeType(n.Type) {
	case "vless", "hy2", "both":
	default:
		return fmt.Errorf("node.type must be vless, hy2 or both (got %q)", n.Type)
	}
	if n.Server.Port < 1 || n.Server.Port > 65535 {
		return fmt.Errorf("node %d server.port must be in 1..65535 (got %d)", n.ID, n.Server.Port)
	}
	return nil
}

func validateNodesFile(nf NodesFile) error {
	if nf.Version != 0 && nf.Version > KnownVersion {
		return fmt.Errorf("unsupported nodes.json version %d", nf.Version)
	}
	if nf.Token == "" {
		return fmt.Errorf("nodes.json token must not be empty")
	}
	if len(nf.Nodes) == 0 {
		return fmt.Errorf("nodes.json must contain at least one node")
	}
	seen := make(map[int]struct{}, len(nf.Nodes))
	for _, n := range nf.Nodes {
		if err := validateNodeEntry(n); err != nil {
			return err
		}
		if _, dup := seen[n.ID]; dup {
			return fmt.Errorf("duplicate node id %d", n.ID)
		}
		seen[n.ID] = struct{}{}
	}
	return nil
}

func validateUsersFile(uf UsersFile) error {
	if uf.Version != 0 && uf.Version > KnownVersion {
		return fmt.Errorf("unsupported users.json version %d", uf.Version)
	}
	if len(uf.Users) > MaxUsers {
		return fmt.Errorf("users.json has %d users, limit is %d", len(uf.Users), MaxUsers)
	}
	seen := make(map[int]struct{}, len(uf.Users))
	for _, u := range uf.Users {
		if err := validateUser(u); err != nil {
			return err
		}
		if _, dup := seen[u.ID]; dup {
			return fmt.Errorf("duplicate user id %d", u.ID)
		}
		seen[u.ID] = struct{}{}
	}
	return nil
}

func validateUser(u User) error {
	if u.ID <= 0 {
		return fmt.Errorf("user id must be positive")
	}
	if u.UUID == "" && u.Password == "" {
		return fmt.Errorf("user %d needs uuid or password", u.ID)
	}
	if u.SpeedLimit < 0 {
		return fmt.Errorf("user %d speed_limit must be >= 0", u.ID)
	}
	return nil
}

// validateTrafficFile checks the schema/version of a traffic document. Negative
// counters are not rejected here: they are sanitized (clamped to zero) by
// normalizeTrafficFile so a bad value can never prevent the server from
// starting or drop the rest of the traffic data.
func validateTrafficFile(tf TrafficFile) error {
	if tf.Version != 0 && tf.Version > KnownVersion {
		return fmt.Errorf("unsupported traffic.json version %d", tf.Version)
	}
	return nil
}

// ---------------------------------------------------------------------------
// saving
// ---------------------------------------------------------------------------

// SaveNode persists the node configuration immediately after any change. In
// multi-node mode it writes nodes.json; otherwise the legacy node.json. The
// snapshot is taken while holding saveMu so snapshot order equals write order:
// an older snapshot can never be promoted after a fresher one.
func (s *Store) SaveNode() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	list := append([]NodeEntry(nil), s.nodeList...)
	token := s.panelToken
	multi := s.nodesMulti
	s.mu.RUnlock()

	if multi {
		data, err := json.MarshalIndent(NodesFile{Version: KnownVersion, Token: token, Nodes: list}, "", "  ")
		if err != nil {
			return err
		}
		return AtomicWriteWithBackup(s.nodesPath(), data, FilePerm)
	}
	nf := NodeFile{Version: KnownVersion}
	if len(list) > 0 {
		nf = nodeFileFromEntry(list[0], token)
	}
	data, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteWithBackup(s.nodePath(), data, FilePerm)
}

// SaveUsers persists users.json immediately after any user change. It never
// holds the state lock while doing I/O: the snapshot is taken under saveMu
// (ordering snapshots against each other) and only uses a brief RLock on the
// state mutex, so concurrent requests are barely blocked.
func (s *Store) SaveUsers() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	snapshot := UsersFile{
		Version: KnownVersion,
		Users:   make([]User, 0, len(s.users)),
	}
	for _, u := range s.users {
		snapshot.Users = append(snapshot.Users, u)
	}
	s.mu.RUnlock()

	sort.Slice(snapshot.Users, func(i, j int) bool { return snapshot.Users[i].ID < snapshot.Users[j].ID })

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteWithBackup(s.usersPath(), data, FilePerm)
}

// SaveTraffic writes the cumulative traffic snapshot (compact JSON, no indent).
// It never clears the dirty flag itself: flushTrafficIfDirty owns that via CAS
// so a push arriving mid-write leaves the flag set and is flushed next tick.
func (s *Store) SaveTraffic() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	tf := TrafficFile{
		Version:   KnownVersion,
		UpdatedAt: time.Now().Unix(),
		Users:     make(map[string]Traffic, len(s.traffic)),
	}
	for id, t := range s.traffic {
		tf.Users[strconv.Itoa(id)] = Traffic{Upload: t.Upload, Download: t.Download}
	}
	s.mu.RUnlock()

	data, err := json.Marshal(tf)
	if err != nil {
		return err
	}
	return AtomicWriteWithBackup(s.trafficPath(), data, FilePerm)
}

// flushTrafficIfDirty persists traffic once when the dirty flag is set. The CAS
// atomically consumes the flag so a concurrent push arriving mid-write re-marks
// it and the next flush retries. Disk errors are logged, never fatal.
func (s *Store) flushTrafficIfDirty() {
	if !s.dirty.CompareAndSwap(true, false) {
		return
	}
	if err := s.SaveTraffic(); err != nil {
		log.Printf("[warn] failed to persist traffic: %v", err)
		s.dirty.Store(true)
	}
}

// RuntimeFlushInterval returns the configured traffic flush interval (seconds).
func (s *Store) RuntimeFlushInterval() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.nodeList {
		if e.Runtime.TrafficFlushInterval > 0 {
			return e.Runtime.TrafficFlushInterval
		}
	}
	return DefaultTrafficFlushInterval
}

// ---------------------------------------------------------------------------
// user mutations
// ---------------------------------------------------------------------------

// SetUsers replaces the whole user list. It validates everything up front,
// prunes traffic for removed users, then persists immediately.
func (s *Store) SetUsers(users []User) error {
	if len(users) > MaxUsers {
		return fmt.Errorf("too many users: %d > %d", len(users), MaxUsers)
	}
	for _, u := range users {
		if err := validateUser(u); err != nil {
			return err
		}
	}
	seen := make(map[int]struct{}, len(users))
	for _, u := range users {
		if _, dup := seen[u.ID]; dup {
			return fmt.Errorf("duplicate user id %d", u.ID)
		}
		seen[u.ID] = struct{}{}
	}

	s.mu.Lock()
	m := make(map[int]User, len(users))
	for _, u := range users {
		m[u.ID] = u
	}
	for id := range s.traffic {
		if _, ok := m[id]; !ok {
			delete(s.traffic, id)
		}
	}
	s.users = m
	s.mu.Unlock()
	return s.SaveUsers()
}

// AddUser inserts a user, enforcing the max user limit and uniqueness by id.
func (s *Store) AddUser(u User) error {
	if err := validateUser(u); err != nil {
		return err
	}
	s.mu.Lock()
	if _, exists := s.users[u.ID]; exists {
		s.mu.Unlock()
		return fmt.Errorf("user %d already exists", u.ID)
	}
	if len(s.users) >= MaxUsers {
		s.mu.Unlock()
		return fmt.Errorf("user limit reached (%d)", MaxUsers)
	}
	s.users[u.ID] = u
	s.mu.Unlock()
	return s.SaveUsers()
}

// DeleteUser removes a user and its traffic, then persists.
func (s *Store) DeleteUser(id int) error {
	s.mu.Lock()
	if _, exists := s.users[id]; !exists {
		s.mu.Unlock()
		return fmt.Errorf("user %d does not exist", id)
	}
	delete(s.users, id)
	delete(s.traffic, id)
	s.mu.Unlock()
	return s.SaveUsers()
}

// EnabledUsers returns the enabled user list sorted by id.
func (s *Store) EnabledUsers() []User {
	s.mu.RLock()
	users := make([]User, 0, len(s.users))
	for _, u := range s.users {
		if u.Enabled {
			users = append(users, u)
		}
	}
	s.mu.RUnlock()
	sort.Slice(users, func(i, j int) bool { return users[i].ID < users[j].ID })
	return users
}

// ---------------------------------------------------------------------------
// traffic accumulation
// ---------------------------------------------------------------------------

// Accumulate adds an upload/download delta for a known user. Unknown users are
// ignored (the caller still proceeds with the rest of the push). It returns
// false when the id is not a current user.
func (s *Store) Accumulate(uid int, upload, download int64) bool {
	if upload < 0 || download < 0 {
		return false
	}
	s.mu.Lock()
	if _, ok := s.users[uid]; !ok {
		s.mu.Unlock()
		return false
	}
	t := s.traffic[uid]
	if t == nil {
		t = &Traffic{}
		s.traffic[uid] = t
	}
	t.Upload += upload
	t.Download += download
	s.mu.Unlock()
	s.dirty.Store(true)
	return true
}

// SnapshotTraffic returns a copy of the cumulative counters keyed by user id.
func (s *Store) SnapshotTraffic() map[int]Traffic {
	s.mu.RLock()
	out := make(map[int]Traffic, len(s.traffic))
	for id, t := range s.traffic {
		out[id] = Traffic{Upload: t.Upload, Download: t.Download}
	}
	s.mu.RUnlock()
	return out
}

// ---------------------------------------------------------------------------
// node helpers
// ---------------------------------------------------------------------------

// NodeInfo reports the first managed node (single-node deployments and tests).
func (s *Store) NodeInfo() NodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.nodeList {
		return NodeInfo{ID: e.ID, Name: e.Name, Type: e.Type, Enabled: e.Enabled}
	}
	return NodeInfo{}
}

// AuthToken returns the shared panel token (通讯密钥).
func (s *Store) AuthToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.panelToken
}

// NormalizedNodeType reports the first managed node's type.
func (s *Store) NormalizedNodeType() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.nodeList) == 0 {
		return ""
	}
	return normalizeNodeType(s.nodeList[0].Type)
}

// lookupNode returns the full node entry for a given id.
func (s *Store) lookupNode(id int) (NodeEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.nodeList {
		if e.ID == id {
			return e, true
		}
	}
	return NodeEntry{}, false
}

// nodeSnapshot is a single-consistent read of one node's identity used by the
// auth hot path, so token/id/type checks can never observe a torn config.
type nodeSnapshot struct {
	Found   bool
	Enabled bool
	Type    string
}

func (s *Store) nodeSnapshot(id int) nodeSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.nodeList {
		if e.ID == id {
			return nodeSnapshot{Found: true, Enabled: e.Enabled, Type: e.Type}
		}
	}
	return nodeSnapshot{}
}

// validToken reports whether token authenticates the given node: an entry-level
// token overrides the shared panel token.
func (s *Store) validToken(token string, id int) bool {
	if token == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.nodeList {
		if e.ID == id {
			if e.Token != "" {
				return token == e.Token
			}
			return token == s.panelToken
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// defaults
// ---------------------------------------------------------------------------

func defaultNode() NodeFile {
	return NodeFile{
		Version: KnownVersion,
		Node: NodeInfo{
			ID:      1,
			Name:    "nat-vless-01",
			Type:    "vless",
			Enabled: true,
		},
		Auth: AuthCfg{Token: randomToken(16)},
		Server: ServerCfg{
			Listen: "0.0.0.0",
			Port:   443,
		},
		Protocol: ProtocolCfg{
			Type:       "vless",
			Network:    "tcp",
			Flow:       "xtls-rprx-vision",
			Decryption: "none",
		},
		TLS: TLSCfg{
			Enabled:       true,
			ServerName:    "www.microsoft.com",
			ServerPort:    443,
			PrivateKey:    "",
			ShortID:       "abcd1234",
			AllowInsecure: true,
		},
		Runtime: RuntimeCfg{
			SyncInterval:         DefaultSyncInterval,
			TrafficFlushInterval: DefaultTrafficFlushInterval,
		},
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "change-me"
	}
	return hex.EncodeToString(b)
}
