package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// FilePerm is used for every persisted JSON document.
	FilePerm os.FileMode = 0600
	// DirPerm is used for the data directory.
	DirPerm os.FileMode = 0700

	// MaxStateFileSize bounds any single state document read off disk so a
	// corrupt/oversized file can never force a huge allocation on a
	// memory-starved container.
	MaxStateFileSize = 16 << 20
)

// SyncDir flushes a directory entry so a rename() made before it is durable.
// Directory fsync is not supported on Windows, so it is skipped there.
func SyncDir(dir string) error {
	if dir == "" || runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// AtomicWriteWithBackup durably replaces path with data using:
//
//	tmp write -> fsync -> old path -> path.bak -> tmp -> path -> fsync dir
//
// The previous generation is kept as path.bak so a later crash corrupting the
// main file can still be recovered. If promoting tmp fails, the backup is
// restored in place. Writes are expected to be serialized by the caller.
func AtomicWriteWithBackup(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp := filepath.Join(dir, base+".tmp")
	bak := filepath.Join(dir, base+".bak")

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	keepTmp := false
	defer func() {
		_ = f.Close()
		if !keepTmp {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// Keep the previous generation as a backup before replacing the main file,
	// so a power-loss window can never leave the main file missing.
	if _, err := os.Stat(path); err == nil {
		if err := os.Rename(path, bak); err != nil {
			return err
		}
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Rename(bak, path) // best-effort restore
		return fmt.Errorf("promote %s: %w", base, err)
	}
	keepTmp = true

	if err := SyncDir(dir); err != nil {
		return err
	}
	return nil
}

// LoadFile reads and decodes a single JSON file. A leading UTF-8 BOM is
// tolerated since some editors (and Windows PowerShell) emit one. The read is
// capped at MaxStateFileSize so a corrupt oversized file cannot exhaust memory.
func LoadFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxStateFileSize+1))
	if err != nil {
		return err
	}
	if len(data) > MaxStateFileSize {
		return fmt.Errorf("file too large (exceeds %d bytes)", MaxStateFileSize)
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	return json.Unmarshal(data, v)
}

// loadWithFallback decodes path, falling back to path.bak when the primary file
// is missing or corrupt. It reports whether the backup was used.
func loadWithFallback(path string, v any) (usedBackup bool, err error) {
	if err := LoadFile(path, v); err == nil {
		return false, nil
	}
	bak := path + ".bak"
	if err := LoadFile(bak, v); err != nil {
		return false, fmt.Errorf("load %s (and backup %s): %w", path, bak, err)
	}
	return true, nil
}
