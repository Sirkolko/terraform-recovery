//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package recovery

// lockDir is a no-op on platforms without a supported locking primitive.
func lockDir(string) (func(), error) { return func() {}, nil }
