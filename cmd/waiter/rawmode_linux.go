//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

func setRawMode(fd int) (func(), error) {
	var old termios
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCGETS, uintptr(unsafe.Pointer(&old))); err != 0 {
		return func() {}, err
	}
	new := old
	new.Lflag &^= ICANON | ECHO
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCSETS, uintptr(unsafe.Pointer(&new))); err != 0 {
		return func() {}, err
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), TCSETS, uintptr(unsafe.Pointer(&old)))
	}, nil
}
