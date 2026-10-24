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

// js/wasm replacement for mount_helper_unix.go: no /proc mountinfo, no uname,
// no mount syscalls.
package mount

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
)

const (
	expectedAtLeastNumFieldsPerMountInfo = 10
	maxListTries                         = 10
)

// IsCorruptedMnt returns true if err is about a corrupted mount point.
func IsCorruptedMnt(err error) bool { return false }

// MountInfo represents a single line in /proc/<pid>/mountinfo.
type MountInfo struct {
	ID             int
	ParentID       int
	Major          int
	Minor          int
	Root           string
	Source         string
	MountPoint     string
	OptionalFields []string
	FsType         string
	MountOptions   []string
	SuperOptions   []string
}

// ParseMountInfo parses /proc/xxx/mountinfo.
func ParseMountInfo(filename string) ([]MountInfo, error) {
	content, err := readMountInfo(filename)
	if err != nil {
		return []MountInfo{}, err
	}
	_ = content
	return []MountInfo{}, nil
}

func splitMountOptions(s string) []string {
	inQuotes := false
	return strings.FieldsFunc(s, func(r rune) bool {
		if r == '"' {
			inQuotes = !inQuotes
		}
		return r == ',' && !inQuotes
	})
}

func isMountPointMatch(mp MountPoint, dir string) bool {
	return strings.TrimSuffix(mp.Path, "\\040(deleted)") == dir
}

// PathExists returns true if the specified path exists.
func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

var (
	hasMountinfoBug       bool
	checkMountinfoBugOnce sync.Once
)

func kernelHasMountinfoBug() bool {
	checkMountinfoBugOnce.Do(func() { hasMountinfoBug = false })
	return hasMountinfoBug
}

func readMountInfo(path string) ([]byte, error) {
	return os.ReadFile(path)
}
