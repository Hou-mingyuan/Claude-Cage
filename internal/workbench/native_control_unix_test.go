//go:build !windows

package workbench

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestNativeControlReusesOneConnectionForPolls(t *testing.T) {
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("needs cat as a stand-in guest connection")
	}
	opened := 0
	var last *nativeWindowConnection
	open := func() (*nativeWindowConnection, error) {
		opened++
		ctx, cancel := context.WithCancel(context.Background())
		last, err = startWindowConnection(exec.CommandContext(ctx, cat), cancel)
		return last, err
	}
	var control nativeControl
	defer control.close()
	for _, line := range []string{"{\"action\":\"files-poll\"}\n", "{\"action\":\"status\"}\n"} {
		reply, ok := control.exchange(context.Background(), open, []byte(line))
		if !ok || string(reply)+"\n" != line {
			t.Fatalf("poll lost on the shared connection: %q %v", reply, ok)
		}
	}
	if opened != 1 {
		t.Fatalf("polls opened %d connections", opened)
	}

	// The guest ends a connection it considers idle. That poll goes out on a
	// connection of its own, and the following one shares a new connection.
	last.Close()
	if _, ok := control.exchange(context.Background(), open, []byte("{}\n")); ok {
		t.Fatal("a closed connection answered")
	}
	if reply, ok := control.exchange(context.Background(), open, []byte("{}\n")); !ok || string(reply) != "{}" || opened != 2 {
		t.Fatalf("no new shared connection after the guest closed one: %q %v %d", reply, ok, opened)
	}

	control.close()
	if last.cmd.ProcessState == nil {
		t.Fatal("closing left the guest connection open")
	}
}

func TestNativeControlStopsWaitingAndBacksOffWhenTheGuestDoesNotAnswer(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("needs sleep as a silent guest connection")
	}
	opened := 0
	var silent *nativeWindowConnection
	open := func() (*nativeWindowConnection, error) {
		opened++
		ctx, cancel := context.WithCancel(context.Background())
		silent, err = startWindowConnection(exec.CommandContext(ctx, sleep, "30"), cancel)
		return silent, err
	}
	var control nativeControl
	defer control.close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, ok := control.exchange(ctx, open, []byte("{}\n")); ok || time.Since(started) > 5*time.Second {
		t.Fatal("a cancelled poll kept waiting for the guest")
	}
	if silent.cmd.ProcessState == nil {
		t.Fatal("a cancelled poll left its connection open")
	}

	failure := errors.New("guest unavailable")
	unavailable := func() (*nativeWindowConnection, error) { opened++; return nil, failure }
	if _, ok := control.exchange(context.Background(), unavailable, []byte("{}\n")); ok {
		t.Fatal("an unavailable guest answered")
	}
	before := opened
	if _, ok := control.exchange(context.Background(), unavailable, []byte("{}\n")); ok || opened != before {
		t.Fatal("polls kept opening connections to an unavailable guest")
	}
}
