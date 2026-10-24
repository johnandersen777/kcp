//go:build js

package hostos

import "os"

// js/wasm has no flock. The browser process owns the only descriptor, so the
// lock is a no-op.
const (
	LockShared      = 1
	LockExclusive   = 2
	LockNonBlocking = 4
)

func Lock(*os.File, int) error { return nil }

func Unlock(*os.File) error { return nil }
