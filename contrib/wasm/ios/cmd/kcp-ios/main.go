//go:build ios

// Command kcp-ios exposes the kcpios package as a C archive for the AT-IRC
// iOS app. Build with:
//
//	GOOS=ios GOARCH=arm64 CGO_ENABLED=1 go build \
//	  -modfile=.go.memory.mod -buildmode=c-archive \
//	  -o libkcpios.a ./contrib/wasm/ios/cmd/kcp-ios
//
// Every returned *C.char is owned by the caller: free it with KCPFree.
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/kcp-dev/kcp/contrib/wasm/ios/kcpios"
)

func main() {}

func result(value string, err error) *C.char {
	if err != nil {
		raw, _ := json.Marshal(map[string]string{"error": err.Error()})
		return C.CString(string(raw))
	}
	return C.CString(value)
}

//export KCPVersion
func KCPVersion() *C.char {
	return C.CString(kcpios.Version())
}

//export KCPStart
func KCPStart(rootDir, snapshot *C.char) *C.char {
	status, err := kcpios.Start(C.GoString(rootDir), C.GoString(snapshot))
	return result(status, err)
}

//export KCPRequest
func KCPRequest(method, path, headersJSON, body *C.char) *C.char {
	out, err := kcpios.Request(C.GoString(method), C.GoString(path), C.GoString(headersJSON), C.GoString(body))
	return result(out, err)
}

//export KCPSnapshot
func KCPSnapshot() *C.char {
	out, err := kcpios.Snapshot()
	return result(out, err)
}

//export KCPReady
func KCPReady() C.int {
	if kcpios.Ready() {
		return 1
	}
	return 0
}

//export KCPStatus
func KCPStatus() *C.char {
	return C.CString(kcpios.StatusJSON())
}

//export KCPStop
func KCPStop() {
	kcpios.Stop()
}

//export KCPFree
func KCPFree(value *C.char) {
	C.free(unsafe.Pointer(value))
}
