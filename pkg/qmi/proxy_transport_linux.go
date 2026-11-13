//go:build linux

package qmi

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	dialProxyHook         = dialProxy
	startProxyProcessHook = startProxyProcess
	proxyRetryDelay       = 100 * time.Millisecond
	proxySpawnMu          sync.Mutex
)

func openProxyTransport(ctx context.Context, opts ClientOptions) (qmiTransport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		timeout := opts.ProxyOpenTimeout
		if timeout <= 0 {
			timeout = defaultProxyOpenTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	proxyPath := opts.ProxyPath
	if proxyPath == "" {
		proxyPath = defaultProxyPath
	}
	proxyExecutable := opts.ProxyExecutable
	if proxyExecutable == "" {
		proxyExecutable = defaultProxyExecutable
	}

	conn, firstErr := dialProxyHook(ctx, proxyPath)
	if firstErr == nil {
		return conn, nil
	}

	if proxyExecutable == "" {
		return nil, fmt.Errorf("connect qmi-proxy %q: %w", proxyPath, firstErr)
	}
	if _, err := os.Stat(proxyExecutable); err != nil {
		return nil, fmt.Errorf("connect qmi-proxy %q failed: %w; proxy executable %s is unavailable: %v", proxyPath, firstErr, proxyExecutable, err)
	}

	proxySpawnMu.Lock()
	conn, retryErr := dialProxyHook(ctx, proxyPath)
	if retryErr == nil {
		proxySpawnMu.Unlock()
		return conn, nil
	}
	startErr := startProxyProcessHook(proxyExecutable)
	proxySpawnMu.Unlock()
	if startErr != nil {
		return nil, fmt.Errorf("connect qmi-proxy %q failed and start %s failed: %w", proxyPath, proxyExecutable, startErr)
	}

	var lastErr error = firstErr
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("connect qmi-proxy %q after starting %s: last error: %v: %w", proxyPath, proxyExecutable, lastErr, err)
		}
		timer := time.NewTimer(proxyRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf("connect qmi-proxy %q after starting %s: last error: %v: %w", proxyPath, proxyExecutable, lastErr, ctx.Err())
		case <-timer.C:
		}
		conn, err := dialProxyHook(ctx, proxyPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
}

func dialProxy(ctx context.Context, proxyPath string) (qmiTransport, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", proxySocketAddress(proxyPath))
}

func proxySocketAddress(proxyPath string) string {
	if proxyPath == "" {
		proxyPath = defaultProxyPath
	}
	if strings.HasPrefix(proxyPath, "\x00") {
		return proxyPath
	}
	if strings.HasPrefix(proxyPath, "@") {
		return "\x00" + strings.TrimPrefix(proxyPath, "@")
	}
	if strings.Contains(proxyPath, "/") {
		return proxyPath
	}
	return "\x00" + proxyPath
}

func startProxyProcess(proxyExecutable string) error {
	cmd := exec.Command(proxyExecutable)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}
