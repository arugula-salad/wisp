package server

import (
	"path/filepath"
)

// The engine half of checkpoints (checkpoints.go has the routes).

func (l *Lifecycle) checkpointPath(id, checkpoint string) string {
	return filepath.Join(l.store.Dir(id), "checkpoints", checkpoint+".ext4")
}
