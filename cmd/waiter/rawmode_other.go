//go:build !linux

package main

import "fmt"

func setRawMode(fd int) (func(), error) {
	return func() {}, fmt.Errorf("raw terminal mode not supported on this platform")
}
