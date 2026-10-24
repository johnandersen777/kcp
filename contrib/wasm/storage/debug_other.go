//go:build !js || !wasm

package storage

import "os"

func debugTimingsEnabled() bool {
	return os.Getenv("KCP_WASM_STORAGE_DEBUG") != ""
}
