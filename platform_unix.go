//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func environmentKey(key string) string {
	return key
}

func gitPathLine(path string) string {
	return path
}

func configureGitCommand(_ *exec.Cmd) {}

func openSnapshotLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}
