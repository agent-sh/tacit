//go:build !unix

package platform

import "os"

// Audit receipts contain private context. Without the Unix ownership and
// no-follow checks, fail closed rather than claim chmod supplies an ACL.
// Utility scoring itself remains available; only optional local audit is off.
func OpenPrivateDirectory(path string) (*os.File, error)            { return nil, os.ErrPermission }
func OpenPrivateAppend(dir *os.File, name string) (*os.File, error) { return nil, os.ErrPermission }
func RemovePrivateEntry(dir *os.File, name string) error            { return os.ErrPermission }
