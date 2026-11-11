//go:build !windows

// This file implements the POSIX side of the two file primitives
// WriteFileAtomic needs. Both are already provided by the platform: rename is
// atomic over an open file, and the mode bits set on the staging file follow it
// to its final name.
package hostpath

import "os"

// replaceFile renames old to new. POSIX rename replaces the target even while a
// reader has it open, so the Windows retry loop is unnecessary.
func replaceFile(old, new string) error { return os.Rename(old, new) }

// secureFile is a no-op: WriteFileAtomic already applied the requested mode to
// the temporary file, and the rename preserves it.
func secureFile(string) error { return nil }

// removeFile deletes path. A POSIX unlink succeeds even while a reader holds the
// file open, so no retry is needed.
func removeFile(path string) error { return os.Remove(path) }

// readFileShared is os.ReadFile: a POSIX read never blocks a concurrent rename.
func readFileShared(path string) ([]byte, error) { return os.ReadFile(path) }
