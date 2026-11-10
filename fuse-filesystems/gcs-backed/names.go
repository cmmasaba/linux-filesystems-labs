package main

import (
	"strings"

	"github.com/winfsp/cgofuse/fuse"
)

const (
	maxComponentLen  = 255  // NAME_MAX on Linux
	maxObjectNameLen = 1024 // GCS limit, in bytes of UTF-8
)

// objectName converts a FUSE path ("/a/b") to a GCS object name ("a/b").
// The root directory is "".
func objectName(path string) string {
	return strings.Trim(path, "/")
}

// dirPrefix is the listing prefix (and placeholder object name) of a directory.
func dirPrefix(name string) string {
	if name == "" {
		return ""
	}

	return name + "/"
}

// parentOf returns the object name of the directory containing name.
func parentOf(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i]
	}

	return ""
}

// validComponent reports whether c can be shown as a POSIX directory entry.
// GCS allows names that POSIX can't represent ("a//b", "a/./b", very long
// segments); the filesystem hides those instead of failing.
func validComponent(c string) bool {
	return c != "" && c != "." && c != ".." && len(c) <= maxComponentLen &&
		!strings.ContainsAny(c, "\x00\r\n")
}

// validName reports whether every component of an object name is valid.
func validName(name string) bool {
	if name == "" {
		return true
	}

	for _, c := range strings.Split(name, "/") {
		if !validComponent(c) {
			return false
		}
	}

	return true
}

// checkNewName returns 0 if name can be created as a file or directory, or a
// negative errno. The +1 leaves room for a directory placeholder's slash.
func checkNewName(name string) int {
	if name == "" {
		return -fuse.EINVAL
	}

	if len(name)+1 > maxObjectNameLen {
		return -fuse.ENAMETOOLONG
	}

	for _, c := range strings.Split(name, "/") {
		if len(c) > maxComponentLen {
			return -fuse.ENAMETOOLONG
		}

		// GCS rejects carriage returns and line feeds in object names.
		if !validComponent(c) {
			return -fuse.EINVAL
		}
	}

	return 0
}

// childName returns the directory entry name for a listed object or prefix
// under the listing prefix, or ok=false if it should be hidden.
func childName(prefix, full string) (string, bool) {
	rest := strings.TrimSuffix(strings.TrimPrefix(full, prefix), "/")
	if !validComponent(rest) {
		return "", false
	}

	return rest, true
}
