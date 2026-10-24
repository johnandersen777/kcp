//go:build js

package portscan

// js/wasm has no process table or /proc. The browser owns its sockets, so
// there is nothing to scan.
func listeners() (map[int][]int, error) { return map[int][]int{}, nil }

func datagramListeners() (map[int][]int, error) { return map[int][]int{}, nil }

func cmdline(int) ([]string, error) { return nil, nil }
