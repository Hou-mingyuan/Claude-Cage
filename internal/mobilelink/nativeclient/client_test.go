package nativeclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

func TestResumeRebindsReclaimedLoopbackListener(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mobile.html"), []byte("<!doctype html><html>ok</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	launchURL, err := c.StartUI(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	base := strings.SplitN(launchURL, "/link-bootstrap", 2)[0]
	resp, err := http.Get(base + "/link-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 before reclaim, got %d", resp.StatusCode)
	}

	// Simulate iOS reclaiming the listening socket while the app is suspended.
	c.mu.Lock()
	oldListener := c.listener
	c.mu.Unlock()
	if err := oldListener.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		cleared := c.listener == nil
		c.mu.Unlock()
		if cleared {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !c.Resume() {
		t.Fatal("Resume failed to rebind reclaimed loopback listener")
	}

	resp, err = http.Get(base + "/link-bootstrap")
	if err != nil {
		t.Fatalf("loopback request failed after Resume: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after Resume, got %d", resp.StatusCode)
	}
}

func TestSuspendBlocksRecoveryAndCancelsInFlightDial(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()

	dialStarted := make(chan struct{}, 1)
	dialCanceled := make(chan struct{}, 1)
	var dials atomic.Int32

	c := &Client{
		lifetime: lifetime,
		cancel:   cancel,
		recoverLink: func(ctx context.Context) (*mobilelink.Client, error) {
			n := dials.Add(1)
			if n == 1 {
				dialStarted <- struct{}{}
				<-ctx.Done()
				dialCanceled <- struct{}{}
				return nil, ctx.Err()
			}
			return nil, errors.New("dial attempted after resume")
		},
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Do(context.Background(), http.MethodGet, "/api/mobile/cli/projects", nil)
		errCh <- err
	}()

	select {
	case <-dialStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight recovery did not start")
	}

	c.Suspend()

	select {
	case <-dialCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Suspend did not cancel in-flight recovery")
	}
	if err := <-errCh; err == nil {
		t.Fatal("expected in-flight recovery to fail after Suspend")
	}

	// While suspended, new reads must fail immediately without calling recoverLink.
	if _, err := c.Do(context.Background(), http.MethodGet, "/api/mobile/cli/projects", nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed while suspended, got %v", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("expected no recovery dial while suspended, got %d dials", got)
	}

	// Resume clears suspension and permits recovery again.
	if !c.Resume() {
		t.Fatal("Resume returned false with no UI listener")
	}
	if got := dials.Load(); got < 2 {
		t.Fatalf("expected Resume to re-enable recovery dial, got %d dials", got)
	}
}
