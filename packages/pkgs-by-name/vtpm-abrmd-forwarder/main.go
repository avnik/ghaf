// SPDX-FileCopyrightText: 2022-2026 TII (SSRC) and the Ghaf contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	// _IOWR(0xa1, 0x00, struct vtpm_proxy_new_dev)
	// struct size: 5 * __u32 = 20 bytes
	vtpmProxyIOCNewDev = 0xC014A100
	vtpmProxyFlagTPM2  = 1

	tpm2SetLocalityCC = 0x20001000
	tpm2SelfTestCC    = 0x00000143
	tpm2GetCapCC      = 0x0000017A
	tpm2GetRandomCC   = 0x0000017B
	tpm2RCRetry       = 0x00000922
	tpm2RCYielded     = 0x00000908
	tpm2RCTesting     = 0x0000090A

	maxTPMPacketSize = 64 * 1024

	helperRestartBackoffMin = 200 * time.Millisecond
	helperRestartBackoffMax = 5 * time.Second
)

var errBackendBusy = errors.New("backend busy")

type vtpmProxyNewDev struct {
	Flags  uint32
	TPMNum uint32
	FD     uint32
	Major  uint32
	Minor  uint32
}

func main() {
	var vmName string
	var backendDevice string
	var linkPath string

	flag.StringVar(&vmName, "vm-name", "", "VM name")
	flag.StringVar(&backendDevice, "backend-device", "/dev/tpmrm0", "Host TPM device")
	flag.StringVar(&linkPath, "link-path", "", "Path exposed to VM qemu args")
	flag.Parse()

	if vmName == "" {
		log.Fatal("--vm-name is required")
	}
	if linkPath == "" {
		log.Fatal("--link-path is required")
	}

    info, err := os.Stat(backendDevice)
    if err != nil {
        log.Fatalf("backend device not available: %v", err)
    }
    if info.Mode()&os.ModeDevice == 0 {
        log.Fatalf("backend path is not a device: %s", backendDevice)
    }

    if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
        log.Fatalf("failed to create runtime directory: %v", err)
    }

    if err := os.RemoveAll(linkPath); err != nil {
        log.Fatalf("failed to cleanup old link: %v", err)
    }

    vtpmCtl, err := os.OpenFile("/dev/vtpmx", os.O_RDWR, 0)
    if err != nil {
        log.Fatalf("failed to open /dev/vtpmx: %v", err)
    }
    defer vtpmCtl.Close()

	newDev, proxyFD, guestTPMPath, err := allocateUsableVTPM(vtpmCtl, vmName)
	if err != nil {
		log.Fatalf("failed to allocate usable vTPM: %v", err)
	}
	defer proxyFD.Close()

	helper, err := startBackendHelper(backendDevice)
	if err != nil {
		log.Fatalf("failed to start backend helper: %v", err)
	}
	defer helper.Close()

	backendLock, err := os.OpenFile("/run/ghaf-vtpm/backend.lock", os.O_CREATE|os.O_RDWR, 0o660)
	if err != nil {
		log.Fatalf("failed to open backend lock: %v", err)
	}
	defer backendLock.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	errCh := make(chan error, 1)
	stopCh := make(chan struct{})
	var once sync.Once
	var debugCmdCount int32

	go func() {
		idleBackoff := 20 * time.Millisecond
		var totalCmd uint64
		var okCmd uint64
		var backendErrCmd uint64
		var proxyWriteErrCmd uint64
		for {
			select {
			case <-stopCh:
				return
			default:
			}

			cmd, err := readTPMPacketProxy(proxyFD)
			if err != nil {
				if isIdleProxyReadError(err) {
					time.Sleep(idleBackoff)
					if idleBackoff < 500*time.Millisecond {
						idleBackoff *= 2
					}
					continue
				}
				idleBackoff = 20 * time.Millisecond
				log.Printf("proxy read failed: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			idleBackoff = 20 * time.Millisecond

			cmdCC := uint32(0)
			if len(cmd) >= 10 {
				cmdCC = binary.BigEndian.Uint32(cmd[6:10])
			}
			cmdName := tpm2CommandName(cmdCC)

			if atomic.LoadInt32(&debugCmdCount) < 10 {
				log.Printf("proxy cmd size=%d cc=0x%08x(%s) bytes=%x", len(cmd), cmdCC, cmdName, cmd)
				atomic.AddInt32(&debugCmdCount, 1)
			}
			totalCmd++

			if isSetLocalityCommand(cmd) {
				if err := writeAll(proxyFD, tpmSuccessResponse(cmd)); err != nil {
					log.Printf("proxy write failed for set-locality: %v", err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				continue
			}

			if isSelfTestCommand(cmd) {
				if err := writeAll(proxyFD, tpmSuccessResponse(cmd)); err != nil {
					log.Printf("proxy write failed for self-test: %v", err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				continue
			}

			start := time.Now()
			resp, err := transactBackend(helper, backendLock, cmdCC, cmd)
			if err != nil {
				backendErrCmd++
				if err := writeAll(proxyFD, tpmRetryResponse(cmd)); err != nil {
					proxyWriteErrCmd++
					log.Printf("proxy write failed while sending retry cc=0x%08x(%s): %v", cmdCC, cmdName, err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				if errors.Is(err, errBackendBusy) {
					log.Printf("backend busy cc=0x%08x(%s) dur=%s, replied TPM2_RC_RETRY", cmdCC, cmdName, time.Since(start).Round(time.Millisecond))
				} else {
					log.Printf("backend transact failed cc=0x%08x(%s) dur=%s (%v), replied TPM2_RC_RETRY", cmdCC, cmdName, time.Since(start).Round(time.Millisecond), err)
					time.Sleep(25 * time.Millisecond)
				}
				continue
			}
			okCmd++

			dur := time.Since(start)
			if dur > 2*time.Second {
				rc := uint32(0)
				if len(resp) >= 10 {
					rc = binary.BigEndian.Uint32(resp[6:10])
				}
				log.Printf("slow backend response cc=0x%08x(%s) rc=0x%08x dur=%s", cmdCC, cmdName, rc, dur.Round(time.Millisecond))
			}

			if atomic.LoadInt32(&debugCmdCount) <= 10 {
				log.Printf("backend resp size=%d", len(resp))
			}

			if err := writeAll(proxyFD, resp); err != nil {
				proxyWriteErrCmd++
				log.Printf("proxy write failed cc=0x%08x(%s) dur=%s: %v", cmdCC, cmdName, dur.Round(time.Millisecond), err)
				time.Sleep(100 * time.Millisecond)
				continue
			}

			if totalCmd%100 == 0 {
				log.Printf("forwarder stats vm=%s total=%d ok=%d backend_err=%d write_err=%d", vmName, totalCmd, okCmd, backendErrCmd, proxyWriteErrCmd)
			}
		}
	}()

	if err := os.Symlink(guestTPMPath, linkPath); err != nil {
		log.Fatalf("failed to create link %s -> %s: %v", linkPath, guestTPMPath, err)
	}

	log.Printf(
		"vm=%s link=%s guest_tpm=%s backend=%s vtpm=(num=%d major=%d minor=%d)",
		vmName,
		linkPath,
		guestTPMPath,
		backendDevice,
		newDev.TPMNum,
		newDev.Major,
		newDev.Minor,
	)
	sdNotify("READY=1")

	select {
	case sig := <-sigCh:
		once.Do(func() { close(stopCh) })
		fmt.Printf("received signal %s, exiting\n", sig.String())
	case err := <-errCh:
		once.Do(func() { close(stopCh) })
		log.Printf("forwarder exiting due to error: %v", err)
		time.Sleep(250 * time.Millisecond)
		os.Exit(1)
	}
}

func sdNotify(state string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}

	addr := sock
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + strings.TrimPrefix(addr, "@")
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		log.Printf("sd_notify dial failed: %v", err)
		return
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(state)); err != nil {
		log.Printf("sd_notify write failed: %v", err)
	}
}

func chmodTPMNode(path string) error {
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}

	grp, err := user.LookupGroup("kvm")
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return err
	}

	return os.Chown(path, -1, gid)
}

func isSetLocalityCommand(cmd []byte) bool {
	if len(cmd) < 10 {
		return false
	}
	cc := binary.BigEndian.Uint32(cmd[6:10])
	return cc == tpm2SetLocalityCC
}

func isSelfTestCommand(cmd []byte) bool {
	if len(cmd) < 10 {
		return false
	}
	cc := binary.BigEndian.Uint32(cmd[6:10])
	return cc == tpm2SelfTestCC
}

func tpmRetryResponse(cmd []byte) []byte {
	resp := make([]byte, 10)
	binary.BigEndian.PutUint16(resp[0:2], binary.BigEndian.Uint16(cmd[0:2]))
	binary.BigEndian.PutUint32(resp[2:6], uint32(len(resp)))
	binary.BigEndian.PutUint32(resp[6:10], tpm2RCRetry)
	return resp
}

func tpmSuccessResponse(cmd []byte) []byte {
	tagA := byte(0x80)
	tagB := byte(0x01)
	if len(cmd) >= 2 {
		tagA = cmd[0]
		tagB = cmd[1]
	}
	return []byte{tagA, tagB, 0x00, 0x00, 0x00, 0x0A, 0x00, 0x00, 0x00, 0x00}
}

func writeAll(f *os.File, buf []byte) error {
	n, err := f.Write(buf)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return io.ErrShortWrite
	}
	return nil
}

func readTPMPacket(f *os.File) ([]byte, error) {
	header := make([]byte, 6)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, err
	}

	want := int(binary.BigEndian.Uint32(header[2:6]))
	if want <= 0 || want > maxTPMPacketSize {
		return nil, fmt.Errorf("invalid TPM packet size: %d", want)
	}
	if want < len(header) {
		return nil, fmt.Errorf("invalid TPM packet size: %d", want)
	}

	out := make([]byte, want)
	copy(out, header)

	if want == len(header) {
		return out, nil
	}

	if _, err := io.ReadFull(f, out[len(header):]); err != nil {
		return nil, err
	}

	return out, nil
}

func readTPMPacketProxy(f *os.File) ([]byte, error) {
	buf := make([]byte, maxTPMPacketSize)
	n, err := f.Read(buf)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, io.EOF
	}
	if n < 6 {
		return nil, fmt.Errorf("short TPM header (%d bytes)", n)
	}

	want := int(binary.BigEndian.Uint32(buf[2:6]))
	if want <= 0 || want > maxTPMPacketSize {
		return nil, fmt.Errorf("invalid TPM packet size: %d", want)
	}
	if want < 6 {
		return nil, fmt.Errorf("invalid TPM packet size: %d", want)
	}
	if want > n {
		return nil, fmt.Errorf("short TPM packet read: got %d bytes, expected %d", n, want)
	}

	out := make([]byte, want)
	copy(out, buf[:want])
	return out, nil
}

func isIdleProxyReadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.EIO) {
		return true
	}
	if strings.Contains(err.Error(), "short TPM header") || strings.Contains(err.Error(), "short TPM packet read") {
		return true
	}
	return false
}

func waitForPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear within %s", path, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitForTPMOpen(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err == nil {
			_ = f.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become openable within %s: %w", path, timeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func allocateUsableVTPM(vtpmCtl *os.File, vmName string) (vtpmProxyNewDev, *os.File, string, error) {
	var lastErr error

	for attempt := 1; attempt <= 8; attempt++ {
		newDev := vtpmProxyNewDev{Flags: vtpmProxyFlagTPM2}
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			vtpmCtl.Fd(),
			uintptr(vtpmProxyIOCNewDev),
			uintptr(unsafe.Pointer(&newDev)),
		)
		if errno != 0 {
			lastErr = errno
			time.Sleep(150 * time.Millisecond)
			continue
		}

		proxyFD := os.NewFile(uintptr(newDev.FD), fmt.Sprintf("vtpm-proxy-%s", vmName))
		if proxyFD == nil {
			lastErr = errors.New("failed to create proxy fd handle")
			time.Sleep(150 * time.Millisecond)
			continue
		}

		guestTPMPath := fmt.Sprintf("/dev/tpm%d", newDev.TPMNum)
		if err := ensureTPMNode(guestTPMPath, newDev.Major, newDev.Minor); err != nil {
			_ = proxyFD.Close()
			return vtpmProxyNewDev{}, nil, "", err
		}
		if err := chmodTPMNode(guestTPMPath); err != nil {
			log.Printf("warning: could not adjust TPM node permissions: %v", err)
		}

		return newDev, proxyFD, guestTPMPath, nil
	}

	if lastErr == nil {
		lastErr = errors.New("vTPM allocation failed")
	}
	return vtpmProxyNewDev{}, nil, "", lastErr
}

func transactBackend(helper *backendHelper, backendLock *os.File, cmdCC uint32, cmd []byte) ([]byte, error) {
	var lastErr error
	lockWait := 1500 * time.Millisecond
	if cmdCC == tpm2GetRandomCC {
		lockWait = 1200 * time.Millisecond
	}

	if err := acquireBackendLock(backendLock, lockWait); err != nil {
		return nil, err
	}
	defer func() {
		_ = syscall.Flock(int(backendLock.Fd()), syscall.LOCK_UN)
	}()

	for attempt := 1; attempt <= 6; attempt++ {
		resp, err := helper.Transact(cmd)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}

		if isTransientRC(resp) {
			lastErr = fmt.Errorf("transient rc=0x%08x", binary.BigEndian.Uint32(resp[6:10]))
			time.Sleep(100 * time.Millisecond)
			continue
		}

		return resp, nil
	}

	if lastErr == nil {
		lastErr = io.ErrUnexpectedEOF
	}
	return nil, lastErr
}

func acquireBackendLock(backendLock *os.File, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(backendLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return errBackendBusy
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func tpm2CommandName(cc uint32) string {
	switch cc {
	case tpm2SetLocalityCC:
		return "SetLocality"
	case tpm2SelfTestCC:
		return "SelfTest"
	case tpm2GetCapCC:
		return "GetCapability"
	case tpm2GetRandomCC:
		return "GetRandom"
	default:
		return "unknown"
	}
}

type backendHelper struct {
	backendDevice string
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	mu            sync.Mutex
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

func isTransientRC(resp []byte) bool {
	if len(resp) < 10 {
		return false
	}
	rc := binary.BigEndian.Uint32(resp[6:10])
	return rc == tpm2RCRetry || rc == tpm2RCYielded || rc == tpm2RCTesting
}

func ensureTPMNode(path string, major, minor uint32) error {
	wantDev := makeLinuxDev(major, minor)

	if info, err := os.Stat(path); err == nil {
		if info.Mode()&os.ModeDevice == 0 {
			return fmt.Errorf("existing path is not a device: %s", path)
		}

		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("failed to inspect existing device node: %s", path)
		}

		if uint64(st.Rdev) == wantDev {
			return nil
		}

		if err := os.Remove(path); err != nil {
			return fmt.Errorf("failed to replace stale TPM node %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	mode := uint32(syscall.S_IFCHR | 0o660)
	dev := int(wantDev)
	if err := syscall.Mknod(path, mode, dev); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}

	return nil
}

func makeLinuxDev(major, minor uint32) uint64 {
	maj := uint64(major)
	min := uint64(minor)
	return (min & 0xff) | ((maj & 0xfff) << 8) | ((min &^ 0xff) << 12) | ((maj &^ 0xfff) << 32)
}
