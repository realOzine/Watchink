//go:build !windows

package store

// osDiskFull reports platform-specific disk-full errors beyond ENOSPC and
// EDQUOT; there are none outside Windows.
func osDiskFull(error) bool { return false }
