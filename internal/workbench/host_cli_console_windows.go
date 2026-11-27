//go:build windows

package workbench

import (
	"context"
	"io"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

func rawCLIConsole(in, out *os.File) (func(), error) {
	fd := int(in.Fd())
	handle := windows.Handle(out.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return nil, err
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	if err = windows.SetConsoleMode(handle, mode|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		_ = term.Restore(fd, state)
		return nil, err
	}
	return func() { restoreCLIDisplay(out); _ = windows.SetConsoleMode(handle, mode); _ = term.Restore(fd, state) }, nil
}

var cancelConsoleRead = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

func pumpCLIInput(ctx context.Context, r io.Reader, send func([]byte) error) error {
	f, ok := r.(*os.File)
	if !ok {
		return copyCLIInput(ctx, r.Read, send)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		return err
	}
	defer windows.CloseHandle(thread)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		// Pipes can use overlapped reads; console reads are synchronous.
		// A cancellation may race just before ReadConsole enters the kernel.
		_ = f.SetReadDeadline(time.Now())
		for {
			_, _, _ = cancelConsoleRead.Call(uintptr(thread))
			select {
			case <-done:
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
	}()
	err = copyCLIInput(ctx, f.Read, send)
	close(done)
	<-finished
	_ = f.SetReadDeadline(time.Time{})
	return err
}
