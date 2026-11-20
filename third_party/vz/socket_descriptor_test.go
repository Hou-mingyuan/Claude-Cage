package vz

import (
	"io"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSocketConnKeepsFrameworkDescriptorOpen(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[1])

	conn, err := socketConnFromBorrowedFD(pair[0])
	// Simulate Virtualization releasing the original connection. This must
	// succeed, and Go must still be able to read from the descriptor it owns.
	closeErr := unix.Close(pair[0])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if closeErr != nil {
		t.Fatalf("framework descriptor was closed early: %v", closeErr)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(pair[1], []byte("a")); err != nil {
		t.Fatal(err)
	}
	var got [1]byte
	if _, err := io.ReadFull(conn, got[:]); err != nil || got[0] != 'a' {
		t.Fatalf("reading from Go's socket: %q, %v", got, err)
	}
}
