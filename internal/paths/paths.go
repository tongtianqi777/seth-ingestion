// Package paths resolves folder settings that may start with "~".
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// The default folders live together under one folder in the home folder. They
// work on any Mac, because "~" is the home folder of whoever runs the program.
const (
	// DefaultDataDir holds the message database and the WhatsApp session.
	DefaultDataDir = "~/seth-ingestion-data/data"
	// DefaultMediaDir is where media goes unless told otherwise.
	DefaultMediaDir = "~/seth-ingestion-data/media"
	// DefaultDBPath is the message database inside the default data folder.
	DefaultDBPath = DefaultDataDir + "/messages.db"
)

// ExpandHome replaces a leading "~" or "~/" with the user's home folder.
// Other paths, including "~name", are returned unchanged. Shells expand "~"
// before a program sees it, but not in environment variables, settings files
// or arguments passed by another program, so the programs do it themselves.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
