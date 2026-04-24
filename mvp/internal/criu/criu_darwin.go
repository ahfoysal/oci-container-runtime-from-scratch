//go:build darwin

// Package criu: macOS stub. CRIU is Linux-only; there's no darwin
// equivalent in the checkpoint/restore space for general process trees.
// The stub exists so `go build ./...` stays green on macOS.
package criu

import "errors"

var errDarwin = errors.New("CRIU checkpoint/restore requires Linux")

// Dump is a darwin stub.
func Dump(pid int, imagesDir string) error { return errDarwin }

// Restore is a darwin stub.
func Restore(imagesDir string) error { return errDarwin }

// Available always returns false on darwin.
func Available() bool { return false }
