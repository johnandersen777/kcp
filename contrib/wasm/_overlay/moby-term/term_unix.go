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

// js/wasm replacement for term_unix.go. There are no TTY ioctls in a WASM
// runtime; all terminal operations are no-ops.
package term

import (
	"errors"
	"io"
	"os"
)

// ErrInvalidState is returned if the state of the terminal is invalid.
var ErrInvalidState = errors.New("Invalid terminal state")

type terminalState struct{}

func stdStreams() (stdIn io.ReadCloser, stdOut, stdErr io.Writer) {
	return os.Stdin, os.Stdout, os.Stderr
}

func getFdInfo(in interface{}) (uintptr, bool) {
	if f, ok := in.(*os.File); ok {
		return f.Fd(), false
	}
	return 0, false
}

func getWinsize(fd uintptr) (*Winsize, error) { return &Winsize{}, nil }

func setWinsize(fd uintptr, ws *Winsize) error { return nil }

func isTerminal(fd uintptr) bool { return false }

func restoreTerminal(fd uintptr, state *State) error { return nil }

func saveState(fd uintptr) (*State, error) { return &State{}, nil }

func disableEcho(fd uintptr, state *State) error { return nil }

func setRawTerminal(fd uintptr) (*State, error) { return &State{}, nil }

func setRawTerminalOutput(fd uintptr) (*State, error) { return &State{}, nil }

func makeRaw(fd uintptr) (*State, error) { return &State{}, nil }
