//go:build windows

package main

// lockFile is a no-op on Windows: mf targets unix agent hosts, and concurrent
// writers there fall back to last-rename-wins on state.json.
func lockFile(string) (func(), error) { return func() {}, nil }
