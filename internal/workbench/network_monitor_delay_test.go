package workbench

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestNetworkMonitorWaitsAfterSlowPhysicalProbe(t *testing.T) {
	for _, probeDuration := range []time.Duration{6 * time.Second, 10 * time.Second} {
		t.Run(probeDuration.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testServer(t)
				markTestVMRunning(t, s)
				previous := physicalProxyInterface
				defer func() { physicalProxyInterface = previous }()
				var mu sync.Mutex
				var starts []time.Time
				probeStarts := func() []time.Time {
					mu.Lock()
					defer mu.Unlock()
					return slices.Clone(starts)
				}
				physicalProxyInterface = func(context.Context) (string, error) {
					mu.Lock()
					starts = append(starts, time.Now())
					first := len(starts) == 1
					mu.Unlock()
					if first {
						time.Sleep(probeDuration)
					}
					return "en0", nil
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { defer close(done); s.monitorNetwork(ctx) }()
				defer func() { cancel(); <-done }()
				synctest.Wait()
				if n := len(probeStarts()); n != 1 {
					t.Fatalf("initial monitor should run immediately, starts=%d", n)
				}
				if !s.op.TryLock() {
					t.Fatal("physical probe held the lifecycle lock")
				}
				s.op.Unlock()
				time.Sleep(probeDuration)
				synctest.Wait()
				if n := len(probeStarts()); n != 1 {
					t.Fatalf("slow probe triggered an immediate catch-up round: %d", n)
				}
				if !s.op.TryLock() {
					t.Fatal("lifecycle lock was not released between rounds")
				}
				s.op.Unlock()
				time.Sleep(5*time.Second - time.Nanosecond)
				synctest.Wait()
				if len(probeStarts()) != 1 {
					t.Fatal("monitor restarted before the full post-round interval")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if got := probeStarts(); len(got) != 2 || got[1].Sub(got[0]) != probeDuration+5*time.Second {
					t.Fatalf("wrong post-round schedule: %v", got)
				}
			})
		})
	}
}
