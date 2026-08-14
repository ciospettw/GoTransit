// Persistenza dello Store: snapshot periodico via encoding/gob (zero
// dipendenze), scrittura atomica tmp+rename, byte di versione per poter
// evolvere il formato. Senza path configurato lo store è solo in memoria.
package stats

import (
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const snapshotVersion byte = 1

type snapshotFile struct {
	Version  byte
	SavedAt  int64
	HalfLife time.Duration
	Buckets  map[key]bucket
}

// Save writes an atomic snapshot to path.
func (s *Store) Save(path string) error {
	s.mu.RLock()
	out := snapshotFile{
		Version:  snapshotVersion,
		SavedAt:  time.Now().Unix(),
		HalfLife: s.HalfLife,
		Buckets:  make(map[key]bucket, len(s.buckets)),
	}
	for k, b := range s.buckets {
		out.Buckets[k] = *b
	}
	s.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(out); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Load merges a snapshot from disk (missing file is not an error).
func (s *Store) Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	var in snapshotFile
	if err := gob.NewDecoder(f).Decode(&in); err != nil {
		return fmt.Errorf("stats snapshot %s: %w", path, err)
	}
	if in.Version != snapshotVersion {
		return fmt.Errorf("stats snapshot %s: unsupported version %d", path, in.Version)
	}
	s.mu.Lock()
	for k, b := range in.Buckets {
		cp := b
		s.buckets[k] = &cp
	}
	s.mu.Unlock()
	return nil
}

// Run snapshots the store every interval until ctx ends (plus a final save).
// No-op when path is empty. Saves are skipped while nothing changed.
func (s *Store) Run(ctx context.Context, path string, interval time.Duration, onErr func(error)) {
	if path == "" {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	save := func() {
		s.mu.Lock()
		dirty := s.dirty
		s.dirty = false
		s.mu.Unlock()
		if !dirty {
			return
		}
		if err := s.Save(path); err != nil && onErr != nil {
			onErr(err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			save()
			return
		case <-tick.C:
			save()
		}
	}
}
