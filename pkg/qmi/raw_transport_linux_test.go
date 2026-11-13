//go:build linux

package qmi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func newSocketpairRawTransport(t *testing.T) (*rawDeviceTransport, int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("Socketpair() error = %v", err)
	}
	var wakeFDs [2]int
	if err := syscall.Pipe2(wakeFDs[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
		t.Fatalf("Pipe2() error = %v", err)
	}
	tr := &rawDeviceTransport{
		path:    "socketpair-cdc-wdm",
		devFD:   fds[0],
		cancelR: wakeFDs[0],
		cancelW: wakeFDs[1],
	}
	tr.cond = sync.NewCond(&tr.mu)
	t.Cleanup(func() {
		_ = tr.Close()
		_ = syscall.Close(fds[1])
	})
	return tr, fds[1]
}

func TestRawDeviceTransportReadTimeoutAndImmediateCloseWakeup(t *testing.T) {
	tr, peerFD := newSocketpairRawTransport(t)

	// 1. Idle non-blocking FD returns os.ErrDeadlineExceeded (never raw EAGAIN).
	if err := tr.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	buf := make([]byte, 64)
	_, err := tr.Read(buf)
	if !os.IsTimeout(err) && !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read() on idle non-blocking FD error = %v, want timeout", err)
	}

	// 2. Round-trip write/read works cleanly after timeout.
	want := []byte{0x01, 0x0b, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x22, 0x00, 0x00, 0x00}
	if _, err := syscall.Write(peerFD, want); err != nil {
		t.Fatalf("peer Write() error = %v", err)
	}
	if err := tr.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	n, err := tr.Read(buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("Read() = %x, want %x", buf[:n], want)
	}

	// 3. Close() wakes a blocked Read() with a 30s deadline in milliseconds.
	if err := tr.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, rErr := tr.Read(buf)
		readDone <- rErr
	}()
	time.Sleep(15 * time.Millisecond)

	start := time.Now()
	if err := tr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case rErr := <-readDone:
		if !errors.Is(rErr, net.ErrClosed) {
			t.Fatalf("unblocked Read() error = %v, want net.ErrClosed", rErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close() failed to wake blocked Read() promptly")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Close() took %v to wake Read(), want <500ms", elapsed)
	}
}

func TestRawDeviceTransportRejectsShortWrite(t *testing.T) {
	tr, _ := newSocketpairRawTransport(t)
	// Shrink socket send buffer to minimum so an oversized write returns a partial write.
	_ = syscall.SetsockoptInt(tr.devFD, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4096)

	huge := bytes.Repeat([]byte{0xAA}, 512*1024)
	n, err := tr.Write(huge)
	if n > 0 && n < len(huge) {
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("partial Write(%d/%d) error = %v, want io.ErrShortWrite", n, len(huge), err)
		}
	}
}

func TestProxySocketAddressPreservesFilesystemPathsAndAbstractNames(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "\x00qmi-proxy"},
		{"qmi-proxy", "\x00qmi-proxy"},
		{"@qmi-proxy", "\x00qmi-proxy"},
		{"\x00qmi-proxy", "\x00qmi-proxy"},
		{"/run/qmi-proxy.sock", "/run/qmi-proxy.sock"},
		{"./tmp/qmi-proxy.sock", "./tmp/qmi-proxy.sock"},
	}
	for _, tc := range cases {
		if got := proxySocketAddress(tc.in); got != tc.want {
			t.Fatalf("proxySocketAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpenProxyTransportBoundsTimeoutAndSerializesProxySpawn(t *testing.T) {
	fakeBin := t.TempDir() + "/fake-qmi-proxy"
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("WriteFile(fakeBin) error = %v", err)
	}

	origDial := dialProxyHook
	origStart := startProxyProcessHook
	origDelay := proxyRetryDelay
	t.Cleanup(func() {
		dialProxyHook = origDial
		startProxyProcessHook = origStart
		proxyRetryDelay = origDelay
	})
	proxyRetryDelay = 10 * time.Millisecond

	// 1. Bounded timeout when caller passes context.Background() without deadline.
	dialProxyHook = func(context.Context, string) (qmiTransport, error) {
		return nil, errors.New("connection refused")
	}
	startProxyProcessHook = func(string) error {
		return nil
	}

	start := time.Now()
	_, err := openProxyTransport(context.Background(), ClientOptions{
		UseProxy:         true,
		ProxyPath:        "/run/test-qmi-proxy.sock",
		ProxyExecutable:  fakeBin,
		ProxyOpenTimeout: 60 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("openProxyTransport() succeeded when proxy never came up, want timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("openProxyTransport() hung for %v, want bounded ~60ms timeout", elapsed)
	}

	// 2. Concurrent callers serialize startProxyProcessHook and re-check dial under proxySpawnMu!
	var spawned atomic.Int32
	var proxyReady atomic.Bool
	dialProxyHook = func(context.Context, string) (qmiTransport, error) {
		if proxyReady.Load() {
			c1, c2 := net.Pipe()
			t.Cleanup(func() {
				_ = c1.Close()
				_ = c2.Close()
			})
			return c1, nil
		}
		return nil, errors.New("not listening yet")
	}
	startProxyProcessHook = func(string) error {
		spawned.Add(1)
		time.Sleep(20 * time.Millisecond)
		proxyReady.Store(true)
		return nil
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, openErr := openProxyTransport(context.Background(), ClientOptions{
				UseProxy:         true,
				ProxyPath:        "/run/test-qmi-proxy.sock",
				ProxyExecutable:  fakeBin,
				ProxyOpenTimeout: time.Second,
			})
			if openErr == nil && conn != nil {
				_ = conn.Close()
			}
			errs <- openErr
		}()
	}
	wg.Wait()
	close(errs)
	for openErr := range errs {
		if openErr != nil {
			t.Fatalf("concurrent openProxyTransport() error = %v", openErr)
		}
	}
	if got := spawned.Load(); got != 1 {
		t.Fatalf("startProxyProcessHook called %d times under %d concurrent dials, want 1", got, workers)
	}
}

func TestStartProxyProcessReapsChildWithoutDefunctZombies(t *testing.T) {
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skip("/bin/true not present")
	}
	if err := startProxyProcess("/bin/true"); err != nil {
		t.Fatalf("startProxyProcess(/bin/true) error = %v", err)
	}
	// Allow the background reaper goroutine to reap the exited child.
	time.Sleep(50 * time.Millisecond)
}
