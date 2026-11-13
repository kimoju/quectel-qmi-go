//go:build linux

package qmi

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	pollIn   int16 = 0x0001
	pollOut  int16 = 0x0004
	pollErr  int16 = 0x0008
	pollHup  int16 = 0x0010
	pollNval int16 = 0x0020

	defaultRawWriteTimeout = 5 * time.Second
)

type pollFd struct {
	Fd      int32
	Events  int16
	Revents int16
}

func ppoll(fds []pollFd, timeout *time.Duration) (int, error) {
	if len(fds) == 0 {
		return 0, nil
	}
	var ts syscall.Timespec
	var tsPtr uintptr
	if timeout != nil {
		ns := timeout.Nanoseconds()
		if ns < 0 {
			ns = 0
		}
		ts = syscall.NsecToTimespec(ns)
		tsPtr = uintptr(unsafe.Pointer(&ts))
	}
	n, _, errno := syscall.Syscall6(
		syscall.SYS_PPOLL,
		uintptr(unsafe.Pointer(&fds[0])),
		uintptr(len(fds)),
		tsPtr,
		0,
		0,
		0,
	)
	runtime.KeepAlive(fds)
	runtime.KeepAlive(&ts)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

// rawDeviceTransport opens raw QMI character devices (such as /dev/cdc-wdm*)
// in O_NONBLOCK mode and uses ppoll(2) with a self-pipe wakeup channel.
// This avoids Go's os.File blocking-mode EAGAIN hazards on character devices
// and allows Close() to immediately unblock any in-flight Read/Write.
type rawDeviceTransport struct {
	path string

	mu           sync.Mutex
	cond         *sync.Cond
	devFD        int
	cancelR      int
	cancelW      int
	readDeadline time.Time
	closing      bool
	activeOps    int
}

func openPlatformRawTransport(devicePath string) (qmiTransport, error) {
	devFD, err := syscall.Open(devicePath, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open device %s: %w", devicePath, err)
	}

	var pipeFDs [2]int
	if err := syscall.Pipe2(pipeFDs[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		_ = syscall.Close(devFD)
		return nil, fmt.Errorf("failed to create wakeup pipe for %s: %w", devicePath, err)
	}

	t := &rawDeviceTransport{
		path:    devicePath,
		devFD:   devFD,
		cancelR: pipeFDs[0],
		cancelW: pipeFDs[1],
	}
	t.cond = sync.NewCond(&t.mu)
	return t, nil
}

func (t *rawDeviceTransport) acquireOp() (devFD int, cancelR int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing || t.devFD < 0 {
		return -1, -1, net.ErrClosed
	}
	t.activeOps++
	return t.devFD, t.cancelR, nil
}

func (t *rawDeviceTransport) releaseOp() {
	t.mu.Lock()
	t.activeOps--
	notify := t.closing && t.activeOps == 0
	t.mu.Unlock()
	if notify {
		t.cond.Broadcast()
	}
}

func (t *rawDeviceTransport) stateSnapshot() (closing bool, deadline time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closing, t.readDeadline
}

func drainWakePipe(fd int) {
	var scratch [32]byte
	for {
		_, err := syscall.Read(fd, scratch[:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return
		}
	}
}

func (t *rawDeviceTransport) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	devFD, cancelR, err := t.acquireOp()
	if err != nil {
		return 0, err
	}
	defer t.releaseOp()

	for {
		closing, deadline := t.stateSnapshot()
		if closing {
			return 0, net.ErrClosed
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}

		n, readErr := syscall.Read(devFD, p)
		if readErr == nil {
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		}
		if readErr == syscall.EINTR {
			continue
		}
		if readErr != syscall.EAGAIN && readErr != syscall.EWOULDBLOCK {
			return 0, &os.PathError{Op: "read", Path: t.path, Err: readErr}
		}

		var timeoutPtr *time.Duration
		var waitDur time.Duration
		if !deadline.IsZero() {
			waitDur = time.Until(deadline)
			if waitDur <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timeoutPtr = &waitDur
		}

		fds := [2]pollFd{
			{Fd: int32(devFD), Events: pollIn},
			{Fd: int32(cancelR), Events: pollIn},
		}
		ready, pollErr := ppoll(fds[:], timeoutPtr)
		if pollErr == syscall.EINTR {
			continue
		}
		if pollErr != nil {
			return 0, &os.PathError{Op: "poll", Path: t.path, Err: pollErr}
		}
		if ready == 0 {
			return 0, os.ErrDeadlineExceeded
		}
		if fds[1].Revents != 0 {
			if closing, _ := t.stateSnapshot(); closing {
				return 0, net.ErrClosed
			}
			drainWakePipe(cancelR)
			continue
		}
		if fds[0].Revents&pollNval != 0 {
			return 0, net.ErrClosed
		}
	}
}

func (t *rawDeviceTransport) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	devFD, cancelR, err := t.acquireOp()
	if err != nil {
		return 0, err
	}
	defer t.releaseOp()

	deadline := time.Now().Add(defaultRawWriteTimeout)
	for {
		if closing, _ := t.stateSnapshot(); closing {
			return 0, net.ErrClosed
		}

		n, writeErr := syscall.Write(devFD, p)
		if writeErr == nil {
			// Raw QMI character devices require each write(2) to carry a complete
			// atomic QMUX frame; never continue with a partial slice.
			if n != len(p) {
				return n, io.ErrShortWrite
			}
			return n, nil
		}
		if writeErr == syscall.EINTR {
			continue
		}
		if writeErr != syscall.EAGAIN && writeErr != syscall.EWOULDBLOCK {
			return 0, &os.PathError{Op: "write", Path: t.path, Err: writeErr}
		}

		waitDur := time.Until(deadline)
		if waitDur <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		fds := [2]pollFd{
			{Fd: int32(devFD), Events: pollOut},
			{Fd: int32(cancelR), Events: pollIn},
		}
		ready, pollErr := ppoll(fds[:], &waitDur)
		if pollErr == syscall.EINTR {
			continue
		}
		if pollErr != nil {
			return 0, &os.PathError{Op: "poll", Path: t.path, Err: pollErr}
		}
		if ready == 0 {
			return 0, os.ErrDeadlineExceeded
		}
		if fds[1].Revents != 0 {
			if closing, _ := t.stateSnapshot(); closing {
				return 0, net.ErrClosed
			}
			drainWakePipe(cancelR)
		}
		if fds[0].Revents&pollNval != 0 {
			return 0, net.ErrClosed
		}
	}
}

func (t *rawDeviceTransport) SetReadDeadline(d time.Time) error {
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return net.ErrClosed
	}
	changed := !t.readDeadline.Equal(d)
	t.readDeadline = d
	cancelW := t.cancelW
	active := t.activeOps > 0
	t.mu.Unlock()

	if changed && active && cancelW >= 0 {
		_, _ = syscall.Write(cancelW, []byte{1})
	}
	return nil
}

func (t *rawDeviceTransport) Close() error {
	t.mu.Lock()
	if t.closing {
		for t.activeOps > 0 {
			t.cond.Wait()
		}
		t.mu.Unlock()
		return nil
	}
	t.closing = true
	cancelW := t.cancelW
	t.cancelW = -1
	t.mu.Unlock()

	// Closing cancelW triggers POLLHUP on cancelR, waking any blocked ppoll immediately.
	if cancelW >= 0 {
		_ = syscall.Close(cancelW)
	}

	t.mu.Lock()
	for t.activeOps > 0 {
		t.cond.Wait()
	}
	devFD := t.devFD
	cancelR := t.cancelR
	t.devFD = -1
	t.cancelR = -1
	t.mu.Unlock()

	if cancelR >= 0 {
		_ = syscall.Close(cancelR)
	}
	if devFD >= 0 {
		if err := syscall.Close(devFD); err != nil {
			return &os.PathError{Op: "close", Path: t.path, Err: err}
		}
	}
	return nil
}
