package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const emuConsoleSocketSuffix = ".sock"

func emuInstanceDir(pid int) string {
	return filepath.Join(emunetDir(), fmt.Sprintf("emu.%d", pid))
}

func emuConsoleSocketPath(pid, console int) string {
	return filepath.Join(emuInstanceDir(pid), fmt.Sprintf("console%d%s", console, emuConsoleSocketSuffix))
}

func attachEmuConsole(cfg EmuConfig) (int, error) {
	if cfg.AttachPID <= 0 {
		return 0, fmt.Errorf("-pid must name a running emu process")
	}
	if cfg.AttachConsole < 0 {
		return 0, fmt.Errorf("-console must be >= 0")
	}
	path := emuConsoleSocketPath(cfg.AttachPID, cfg.AttachConsole)
	conn, err := net.Dial("unix", path)
	if err != nil {
		return 0, fmt.Errorf("attach console %d for pid %d at %s: %w", cfg.AttachConsole, cfg.AttachPID, path, err)
	}
	defer conn.Close()

	restore, raw, err := enableRawTerminal(cfg.Stdin)
	if err != nil {
		return 0, err
	}
	if raw {
		defer func() { _ = restore() }()
	}

	inErrCh := make(chan error, 1)
	outErrCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, cfg.Stdin)
		if closeWrite, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = closeWrite.CloseWrite()
		}
		inErrCh <- err
	}()
	go func() {
		_, err := io.Copy(cfg.Stdout, conn)
		outErrCh <- err
	}()

	select {
	case err = <-outErrCh:
		_ = conn.Close()
	case err = <-inErrCh:
		if err != nil {
			_ = conn.Close()
			<-outErrCh
			break
		}
		err = <-outErrCh
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return 0, nil
	}
	return 0, err
}

func listEmuInstances(w io.Writer) error {
	if w == nil {
		w = io.Discard
	}
	dirs, err := filepath.Glob(filepath.Join(emunetDir(), "emu.*"))
	if err != nil {
		return err
	}
	sort.Strings(dirs)
	fmt.Fprintln(w, "PID\tSTATUS\tCONSOLES\tDIR")
	for _, dir := range dirs {
		base := filepath.Base(dir)
		rawPID, ok := strings.CutPrefix(base, "emu.")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(rawPID)
		if err != nil || pid <= 0 {
			continue
		}
		status := "running"
		if !processAlive(pid) {
			status = "stale"
		}
		consoles := emuConsoleIndexes(dir)
		if len(consoles) == 0 && status == "stale" {
			continue
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", pid, status, formatConsoleIndexes(consoles), dir)
	}
	return nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func emuConsoleIndexes(dir string) []int {
	matches, _ := filepath.Glob(filepath.Join(dir, "console*"+emuConsoleSocketSuffix))
	out := make([]int, 0, len(matches))
	for _, path := range matches {
		name := filepath.Base(path)
		raw, ok := strings.CutPrefix(name, "console")
		if !ok {
			continue
		}
		raw, ok = strings.CutSuffix(raw, emuConsoleSocketSuffix)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func formatConsoleIndexes(indexes []int) string {
	if len(indexes) == 0 {
		return "-"
	}
	parts := make([]string, len(indexes))
	for i, idx := range indexes {
		parts[i] = strconv.Itoa(idx)
	}
	return strings.Join(parts, ",")
}

type emuConsoleSocket struct {
	index int
	path  string
	rxCh  chan byte
	ln    *net.UnixListener

	outCh chan byte
	done  chan struct{}
	once  sync.Once

	mu   sync.Mutex
	conn *net.UnixConn
}

func newEmuConsoleSocket(index int, rxCh chan byte) (*emuConsoleSocket, error) {
	path := emuConsoleSocketPath(os.Getpid(), index)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	addr := &net.UnixAddr{Name: path, Net: "unix"}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	c := &emuConsoleSocket{
		index: index,
		path:  path,
		rxCh:  rxCh,
		ln:    ln,
		outCh: make(chan byte, 64*1024),
		done:  make(chan struct{}),
	}
	if err := c.writeInfoFile(); err != nil {
		_ = c.Close()
		return nil, err
	}
	go c.acceptLoop()
	go c.writeLoop()
	writeTerminalStatusf("console%d: %s", index, path)
	return c, nil
}

func (c *emuConsoleSocket) writeInfoFile() error {
	path := filepath.Join(filepath.Dir(c.path), "info")
	text := fmt.Sprintf("pid=%d\nstarted=%s\nconsole%d=%s\n", os.Getpid(), time.Now().Format(rfc3339MsecTz0), c.index, c.path)
	return os.WriteFile(path, []byte(text), 0600)
}

func (c *emuConsoleSocket) WriteByte(b byte) {
	select {
	case <-c.done:
	case c.outCh <- b:
	default:
	}
}

func (c *emuConsoleSocket) Close() error {
	var err error
	c.once.Do(func() {
		close(c.done)
		err = c.ln.Close()
		c.closeActiveConn(nil)
		_ = os.Remove(c.path)
		_ = os.Remove(filepath.Join(filepath.Dir(c.path), "info"))
		_ = os.Remove(filepath.Dir(c.path))
	})
	return err
}

func (c *emuConsoleSocket) GuestClose() error {
	c.closeActiveConn(nil)
	return nil
}

func (c *emuConsoleSocket) acceptLoop() {
	for {
		conn, err := c.ln.AcceptUnix()
		if err != nil {
			select {
			case <-c.done:
			default:
				writeTerminalStatusf("console%d: accept: %v", c.index, err)
			}
			return
		}
		c.replaceActiveConn(conn)
		go c.readLoop(conn)
	}
}

func (c *emuConsoleSocket) replaceActiveConn(conn *net.UnixConn) {
	c.mu.Lock()
	old := c.conn
	c.conn = conn
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (c *emuConsoleSocket) closeActiveConn(conn *net.UnixConn) {
	c.mu.Lock()
	if conn == nil || c.conn == conn {
		if c.conn != nil {
			_ = c.conn.Close()
		}
		c.conn = nil
	}
	c.mu.Unlock()
}

func (c *emuConsoleSocket) readLoop(conn *net.UnixConn) {
	defer c.closeActiveConn(conn)
	var buf [1024]byte
	for {
		n, err := conn.Read(buf[:])
		for i := 0; i < n; i++ {
			select {
			case <-c.done:
				return
			case c.rxCh <- buf[i]:
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *emuConsoleSocket) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case b, ok := <-c.outCh:
			if !ok {
				return
			}
			conn := c.activeConn()
			if conn == nil {
				continue
			}
			if _, err := conn.Write([]byte{b}); err != nil {
				c.closeActiveConn(conn)
			}
		}
	}
}

func (c *emuConsoleSocket) activeConn() *net.UnixConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}
