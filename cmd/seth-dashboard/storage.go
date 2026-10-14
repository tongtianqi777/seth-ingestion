package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Storage is what the ingestion takes up on disk and what is left on the
// computer. The sizes are real disk files, not the sizes the database records.
type Storage struct {
	MediaDir   string `json:"media_dir"`
	MediaBytes int64  `json:"media_bytes"` // everything under the media folder
	MediaFiles int    `json:"media_files"` // files found there
	DataDir    string `json:"data_dir"`
	DataBytes  int64  `json:"data_bytes"` // database, its WAL and the WhatsApp session
	UsedBytes  int64  `json:"used_bytes"` // media + data
	DiskTotal  uint64 `json:"disk_total"` // size of the disk holding the media folder
	DiskFree   uint64 `json:"disk_free"`  // space still available to this user
	DiskPath   string `json:"disk_path"`  // the folder whose disk was measured
	Error      string `json:"error,omitempty"`
}

// dirSize adds up the regular files under dir. A missing folder is empty.
func dirSize(dir string) (bytes int64, files int) {
	filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil // unreadable or vanished entries are skipped, not fatal
		}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			bytes += info.Size()
			files++
		}
		return nil
	})
	return
}

// diskSpace measures the disk holding path, or the nearest folder above it
// that exists (the media folder may not have been created yet).
func diskSpace(path string) (total, free uint64, at string, err error) {
	for p := path; ; p = filepath.Dir(p) {
		var st syscall.Statfs_t
		if err = syscall.Statfs(p, &st); err == nil {
			return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize), p, nil
		}
		if _, serr := os.Stat(p); serr == nil || filepath.Dir(p) == p {
			return 0, 0, p, err
		}
	}
}

// storageCache keeps folder walks from running on every 5-second poll; a
// folder of many thousands of files takes a moment to add up.
type storageCache struct {
	mu  sync.Mutex
	key string
	at  time.Time
	val Storage
}

const storageTTL = 30 * time.Second

func (c *storageCache) get(mediaDir, dataDir string) Storage {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := mediaDir + "\x00" + dataDir
	if c.key != key || time.Since(c.at) > storageTTL {
		c.val = readStorage(mediaDir, dataDir)
		c.key, c.at = key, time.Now()
	}
	return c.val
}

func readStorage(mediaDir, dataDir string) Storage {
	s := Storage{MediaDir: mediaDir, DataDir: dataDir}
	s.MediaBytes, s.MediaFiles = dirSize(mediaDir)
	s.DataBytes, _ = dirSize(dataDir)
	s.UsedBytes = s.MediaBytes + s.DataBytes
	var err error
	if s.DiskTotal, s.DiskFree, s.DiskPath, err = diskSpace(mediaDir); err != nil {
		s.Error = "couldn't read the free space: " + err.Error()
	}
	return s
}
