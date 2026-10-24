//go:build js

/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// js/wasm replacement for journal_unix.go. There is no systemd journal in a
// WASM runtime; log to stderr instead.
package journal

import (
	"fmt"
	"os"
)

func Enabled() bool { return false }

func StderrIsJournalStream() (bool, error) { return false, nil }

func StdoutIsJournalStream() (bool, error) { return false, nil }

func Send(message string, priority Priority, vars map[string]string) error {
	_, err := fmt.Fprintln(os.Stderr, message)
	return err
}
