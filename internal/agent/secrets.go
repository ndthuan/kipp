package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// secretFilesDir is where the agent writes the file secrets of its stages:
// its own directory, outside every workspace, apart from another agent's
// on the machine by the workdir it serves.
func (c Config) secretFilesDir() string { return c.scratchDir("secrets") }

// tmpDir is where the agent makes the KIPP_TMP of its stages, in the same
// way.
func (c Config) tmpDir() string { return c.scratchDir("tmp") }

func (c Config) scratchDir(kind string) string {
	abs, err := filepath.Abs(c.Workdir)
	if err != nil {
		abs = c.Workdir
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(os.TempDir(), "kipp-agent-"+kind+"-"+hex.EncodeToString(sum[:6]))
}
