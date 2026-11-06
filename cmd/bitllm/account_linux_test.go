package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPty returns both ends of a new pseudo terminal, with echo on as a shell
// leaves it.
func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty here: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Skipf("unlock pty: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Skipf("pty number: %v", e)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open pty: %v", err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

// On a terminal the words are read and never echoed back to the screen.
func TestReadWordsOnATerminalDoesNotEcho(t *testing.T) {
	master, slave := openPty(t)
	const words = "legal winner thank year wave sausage"

	echoed := make(chan string, 1)
	go func() {
		var seen strings.Builder
		buf := make([]byte, 256)
		for {
			n, err := master.Read(buf)
			seen.Write(buf[:n])
			if err != nil {
				echoed <- seen.String()
				return
			}
		}
	}()

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readWords("words: ", slave, slave)
		done <- result{line, err}
	}()
	// Typed only once the prompt's read has turned echo off.
	time.Sleep(200 * time.Millisecond)
	if _, err := master.WriteString(words + "\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || strings.TrimSpace(r.line) != words {
			t.Fatalf("readWords = %q, %v", r.line, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readWords did not return")
	}
	slave.Close()
	master.SetReadDeadline(time.Now().Add(time.Second))
	select {
	case s := <-echoed:
		if strings.Contains(s, "legal") {
			t.Fatalf("the words were echoed: %q", s)
		}
		if !strings.Contains(s, "words: ") {
			t.Fatalf("no prompt: %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reading the terminal's output did not end")
	}
}
