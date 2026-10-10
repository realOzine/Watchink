//go:build !windows

package media

// osDiskFull reports platform-specific "volume full" errors beyond ENOSPC and
// EDQUOT; only Windows has any (diskfull_windows.go).
func osDiskFull(err error) bool { return false }
