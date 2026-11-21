package workbench

import (
	"fmt"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// Keep stage measurements in memory until the round ends so log writes do not
// consume the time available to the next probe or the lease renewal.
type monitorTiming struct {
	started, checkpoint time.Time
	until, verified     time.Time
	result              string
	stages              []string
}

func (s *Server) startMonitorTiming() *monitorTiming {
	now := time.Now()
	timing := &monitorTiming{started: now, checkpoint: now, result: "completed"}
	s.mu.Lock()
	timing.verified = s.safety.VerifiedAt
	if s.safety.Gate != nil {
		timing.until = s.safety.Gate.Until
	}
	s.mu.Unlock()
	return timing
}

func (t *monitorTiming) stage(name string) {
	now := time.Now()
	t.stages = append(t.stages, fmt.Sprintf("%s_ms=%d", name, now.Sub(t.checkpoint).Milliseconds()))
	t.checkpoint = now
}

// Steady rounds repeat every few seconds. A round with one of these results
// is recorded when the result changes, when it is slow, when the lease margin
// is short, and otherwise once per monitorTimingQuietInterval with the number
// of rounds left out. Every other result (failures, skips under a busy lock,
// pauses) is always recorded.
var monitorQuietResults = map[string]bool{"completed": true, "not_due": true, "renewed": true, "environment_not_ready": true, "job_running": true, "retry_not_due": true}

const (
	monitorTimingQuietInterval = 10 * time.Minute
	monitorTimingSlow          = 2 * time.Second
	monitorTimingShortLease    = 30 * time.Second
)

type monitorTimingLog struct {
	result     string
	recorded   time.Time
	suppressed int
}

func (s *Server) recordMonitorTiming(kind string, timing *monitorTiming) {
	if s.profile == "" {
		return
	}
	finished := time.Now()
	age, remainingStart, remainingEnd := int64(-1), int64(-1), int64(-1)
	if !timing.verified.IsZero() {
		age = timing.started.Sub(timing.verified).Milliseconds()
	}
	if !timing.until.IsZero() {
		remainingStart = timing.until.Sub(timing.started).Milliseconds()
		remainingEnd = timing.until.Sub(finished).Milliseconds()
	}
	duration := finished.Sub(timing.started)
	quiet := monitorQuietResults[timing.result] && duration < monitorTimingSlow &&
		(timing.until.IsZero() || timing.until.Sub(finished) >= monitorTimingShortLease)
	s.monitorTimingMu.Lock()
	if s.monitorTimingLogs == nil {
		s.monitorTimingLogs = map[string]monitorTimingLog{}
	}
	last := s.monitorTimingLogs[kind]
	if quiet && last.result == timing.result && finished.Sub(last.recorded) < monitorTimingQuietInterval && !last.recorded.After(finished) {
		last.suppressed++
		s.monitorTimingLogs[kind] = last
		s.monitorTimingMu.Unlock()
		return
	}
	suppressed := last.suppressed
	s.monitorTimingLogs[kind] = monitorTimingLog{result: timing.result, recorded: finished}
	s.monitorTimingMu.Unlock()
	_ = diagnostics.New(s.profile).Record("info", "network-monitor", fmt.Sprintf(
		"monitor=%s started=%s duration_ms=%d result=%s verified_age_ms=%d lease_known=%t lease_remaining_start_ms=%d lease_remaining_end_ms=%d unrecorded_steady_rounds=%d %s",
		kind, timing.started.UTC().Format(time.RFC3339Nano), duration.Milliseconds(),
		timing.result, age, !timing.until.IsZero(), remainingStart, remainingEnd, suppressed, strings.Join(timing.stages, " ")))
}
