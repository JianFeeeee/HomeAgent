//go:build !linux && !windows

package main

func setRawMode(fd int) (func(), error) {
	return func() {}, nil
}

func isTerminal(fd int) bool {
	return false
}
