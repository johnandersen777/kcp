//go:build js && wasm

package storage

import "syscall/js"

// debugTimingsEnabled uses a JS global because Go's js/wasm port does not
// expose the host environment. Hosts set
// globalThis.__kcpStorageDebug = true before starting the module.
func debugTimingsEnabled() bool {
	value := js.Global().Get("__kcpStorageDebug")
	return value.Type() == js.TypeBoolean && value.Bool()
}
