// Package atircgo is the single Go library embedded in the AT-IRC iOS app.
// It combines the app's IRC helpers with the in-process kcp control plane so
// the process has exactly one Go runtime: iOS cannot link two gomobile
// frameworks safely.
package atircgo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Version reports the Go runtime version, proving the Go runtime is live.
func Version() string {
	return runtime.Version()
}

// Echo trims surrounding whitespace and returns a greeting.
func Echo(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "anonymous"
	}
	return "hello, " + name + " from Go"
}

// SHA256Hex returns the hex-encoded SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var errEmptyLine = errors.New("empty line")

// ParseCommand splits an IRC-style line into its command and arguments.
// Returning an error verifies how Go errors surface in Swift.
func ParseCommand(line string) (string, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", errEmptyLine
	}
	return fields[0], nil
}

// Counter demonstrates exported-struct and method binding.
type Counter struct {
	Count int
}

// NewCounter returns a counter starting at zero.
func NewCounter() *Counter { return &Counter{} }

// Increment adds one and returns the new count.
func (c *Counter) Increment() int {
	c.Count++
	return c.Count
}

var record struct {
	mu   sync.Mutex
	last string
}

// RecordAfter records message from a Go goroutine after a short delay.
func RecordAfter(message string) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		record.mu.Lock()
		record.last = message
		record.mu.Unlock()
	}()
}

// LastRecorded returns the most recent value written by RecordAfter.
func LastRecorded() string {
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.last
}

// Callback is implemented in Swift so Go can deliver events to the app.
type Callback interface {
	OnEvent(message string)
}

// EmitLater invokes cb.OnEvent from a Go goroutine after a short delay.
func EmitLater(cb Callback, message string) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		cb.OnEvent(message)
	}()
}
