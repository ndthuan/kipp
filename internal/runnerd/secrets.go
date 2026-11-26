package runnerd

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

func (c Config) secretFilesDir() string { return c.scratchDir("secrets") }

func (c Config) tmpDir() string { return c.scratchDir("tmp") }

func (c Config) scratchDir(kind string) string {
	abs, err := filepath.Abs(c.Workdir)
	if err != nil {
		abs = c.Workdir
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(os.TempDir(), "kipp-runner-"+kind+"-"+hex.EncodeToString(sum[:6]))
}
