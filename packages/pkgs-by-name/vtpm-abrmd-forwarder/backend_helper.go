// SPDX-FileCopyrightText: 2022-2026 TII (SSRC) and the Ghaf contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type backendHelper struct {
	backendDevice  string
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	mu             sync.Mutex
	restartBackoff time.Duration
	nextRestartAt  time.Time
}

func startBackendHelper(backendDevice string) (*backendHelper, error) {
	helperPath := filepath.Join(filepath.Dir(os.Args[0]), "vtpm-tcti-device-helper")
	cmd := exec.Command(helperPath, backendDevice)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		scanner := make([]byte, 4096)
		for {
			n, err := stderr.Read(scanner)
			if n > 0 {
				log.Printf("backend-helper: %s", string(scanner[:n]))
			}
			if err != nil {
				return
			}
		}
	}()

	return &backendHelper{backendDevice: backendDevice, cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

func (h *backendHelper) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeLocked()
}

func (h *backendHelper) closeLocked() {
	if h.stdin != nil {
		_ = h.stdin.Close()
		h.stdin = nil
	}
	if h.stdout != nil {
		_ = h.stdout.Close()
		h.stdout = nil
	}
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_, _ = h.cmd.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			_ = h.cmd.Process.Kill()
			<-done
		}
	}
	h.cmd = nil
}

func (h *backendHelper) restartLocked() error {
	now := time.Now()
	if !h.nextRestartAt.IsZero() && now.Before(h.nextRestartAt) {
		return fmt.Errorf("backend helper restart cooling down (%s)", time.Until(h.nextRestartAt).Round(10*time.Millisecond))
	}

	h.closeLocked()

	next, err := startBackendHelper(h.backendDevice)
	if err != nil {
		if h.restartBackoff == 0 {
			h.restartBackoff = helperRestartBackoffMin
		} else {
			h.restartBackoff *= 2
			if h.restartBackoff > helperRestartBackoffMax {
				h.restartBackoff = helperRestartBackoffMax
			}
		}
		h.nextRestartAt = now.Add(h.restartBackoff)
		return fmt.Errorf("backend helper restart failed: %w (next retry in %s)", err, h.restartBackoff)
	}

	h.cmd = next.cmd
	h.stdin = next.stdin
	h.stdout = next.stdout
	h.restartBackoff = 0
	h.nextRestartAt = time.Time{}
	return nil
}

func (h *backendHelper) Transact(req []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cmd == nil || !processAlive(h.cmd) {
		if err := h.restartLocked(); err != nil {
			return nil, err
		}
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := h.transactOnceLocked(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt == 1 {
			if rerr := h.restartLocked(); rerr != nil {
				return nil, fmt.Errorf("backend helper transact failed: %v; restart failed: %v", err, rerr)
			}
		}
	}

	return nil, lastErr
}

func processAlive(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	err := cmd.Process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (h *backendHelper) transactOnceLocked(req []byte) ([]byte, error) {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(req)))
	if err := writeBytes(h.stdin, lenBuf); err != nil {
		return nil, err
	}
	if err := writeBytes(h.stdin, req); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(h.stdout, lenBuf); err != nil {
		return nil, err
	}
	respLen := int(binary.BigEndian.Uint32(lenBuf))
	if respLen <= 0 || respLen > maxTPMPacketSize {
		return nil, fmt.Errorf("invalid backend helper response size: %d", respLen)
	}

	resp := make([]byte, respLen)
	if _, err := io.ReadFull(h.stdout, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

func writeBytes(w io.Writer, buf []byte) error {
	written := 0
	for written < len(buf) {
		n, err := w.Write(buf[written:])
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	return nil
}
