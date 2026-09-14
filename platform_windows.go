package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func environmentKey(key string) string {
	return strings.ToUpper(key)
}

func gitPathLine(path string) string {
	return strings.TrimSuffix(path, "\r")
}

func configureGitCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func openSnapshotLock(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot lock path: %w", err)
	}
	// Disallow sharing while this process owns the lock handle.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return nil, errors.Join(errors.New("Cannot open the snapshot lock handle."), syscall.CloseHandle(handle))
	}
	return file, nil
}
