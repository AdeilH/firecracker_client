//go:build !unix

package main

import (
	"os"
	"path/filepath"
)

// lockDir is a best-effort fallback for platforms without flock(2): it only
// creates the lock file. State writes stay atomic regardless (tmp + rename).
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}
