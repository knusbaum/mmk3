// Package lib bundles mmk's standard library `.mmk` files as an embedded
// fs.FS. They're extracted lazily to a per-process temp directory on first
// access via Dir() so that include resolution (cmd/mmk/parse) can reach
// them through the regular MMK_LIB_PATH search mechanism. This makes
// `go install github.com/knusbaum/mmk3/cmd/mmk@...` ship a self-contained
// binary — no separate lib/ directory or env-var setup required.
package lib

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

//go:embed *.mmk
var fsys embed.FS

var (
	dirOnce sync.Once
	dirPath string
	dirErr  error
)

// Dir returns the path to a directory containing the embedded stdlib files.
// On first call, the embedded files are extracted to a process-lifetime temp
// directory; subsequent calls return the cached path. Returns an error if the
// extraction failed.
func Dir() (string, error) {
	dirOnce.Do(func() {
		tmp, err := os.MkdirTemp("", "mmk-stdlib-")
		if err != nil {
			dirErr = fmt.Errorf("create stdlib temp dir: %w", err)
			return
		}
		entries, err := fsys.ReadDir(".")
		if err != nil {
			dirErr = fmt.Errorf("read embedded stdlib: %w", err)
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := fsys.ReadFile(e.Name())
			if err != nil {
				dirErr = fmt.Errorf("read embedded %s: %w", e.Name(), err)
				return
			}
			if err := os.WriteFile(filepath.Join(tmp, e.Name()), data, 0o644); err != nil {
				dirErr = fmt.Errorf("extract %s: %w", e.Name(), err)
				return
			}
		}
		dirPath = tmp
	})
	return dirPath, dirErr
}
