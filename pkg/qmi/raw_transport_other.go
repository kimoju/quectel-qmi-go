//go:build !linux

package qmi

import (
	"fmt"
	"io"
	"os"
	"time"
)

type rawFileTransport struct {
	file *os.File
}

func openPlatformRawTransport(devicePath string) (qmiTransport, error) {
	file, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open device %s: %w", devicePath, err)
	}
	return &rawFileTransport{file: file}, nil
}

func (t *rawFileTransport) Read(p []byte) (int, error) {
	return t.file.Read(p)
}

func (t *rawFileTransport) Write(p []byte) (int, error) {
	n, err := t.file.Write(p)
	if err == nil && n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, err
}

func (t *rawFileTransport) Close() error {
	return t.file.Close()
}

func (t *rawFileTransport) SetReadDeadline(d time.Time) error {
	return t.file.SetReadDeadline(d)
}
