package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteCreatesFileAndNoTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")

	if err := AtomicWriteWithBackup(path, []byte(`{"version":1,"users":[]}`), FilePerm); err != nil {
		t.Fatalf("first write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"version":1,"users":[]}` {
		t.Fatalf("content = %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(dir, "users.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file should be cleaned up, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "users.json.bak")); !os.IsNotExist(err) {
		t.Fatalf("backup should not exist on first write, err=%v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != os.FileMode(0600) {
			t.Fatalf("perm = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestAtomicWriteKeepsPreviousGenerationInBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.json")

	if err := AtomicWriteWithBackup(path, []byte("generation-one"), FilePerm); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteWithBackup(path, []byte("generation-two"), FilePerm); err != nil {
		t.Fatal(err)
	}

	main, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(main) != "generation-two" {
		t.Fatalf("main = %q", string(main))
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != "generation-one" {
		t.Fatalf("backup = %q, want previous generation", string(bak))
	}
}

func TestLoadFileToleratesUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bom.json")
	bom := []byte{0xEF, 0xBB, 0xBF}
	data := append(bom, []byte(`{"a":42}`)...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var v map[string]int
	if err := LoadFile(path, &v); err != nil {
		t.Fatalf("BOM-prefixed JSON should load: %v", err)
	}
	if v["a"] != 42 {
		t.Fatalf("v = %+v", v)
	}
}

func TestLoadWithFallbackPrefersMainAndFallsBackToBak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")

	// main valid -> used, no backup involved
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	var v1 map[string]int
	used, err := loadWithFallback(path, &v1)
	if err != nil || used {
		t.Fatalf("main-only load: used=%v err=%v", used, err)
	}
	if v1["a"] != 1 {
		t.Fatalf("v1 = %+v", v1)
	}

	// main corrupt, backup valid -> from backup
	if err := os.WriteFile(path, []byte(`{not json`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak", []byte(`{"a":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	var v2 map[string]int
	used, err = loadWithFallback(path, &v2)
	if err != nil || !used {
		t.Fatalf("fallback load: used=%v err=%v", used, err)
	}
	if v2["a"] != 2 {
		t.Fatalf("v2 = %+v", v2)
	}

	// both corrupt -> error
	if err := os.WriteFile(path+".bak", []byte(`nope`), 0600); err != nil {
		t.Fatal(err)
	}
	var v3 map[string]int
	if _, err := loadWithFallback(path, &v3); err == nil {
		t.Fatal("expected error when main and backup are both corrupt")
	}
}
