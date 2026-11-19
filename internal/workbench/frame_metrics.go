package workbench

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

// frameMetricsWindow bounds how often a busy window view writes a summary.
// Summaries are written only when a window elapses during activity, never on
// disconnect: a stream may close while its profile directory is being removed.
const frameMetricsWindow = 10 * time.Second

// frameMetrics summarizes one guest window stream: how long the guest waited
// for and encoded each frame, the transport, and how long the page took to
// acknowledge it. It records timings and sizes only, never window content.
// A nil *frameMetrics records nothing.
type frameMetrics struct {
	mu        sync.Mutex
	surface   string
	record    func(string)
	started   time.Time
	frames    int
	skipped   int
	cached    int
	bytes     int64
	request   []float64
	target    []float64
	layout    []float64
	capture   []float64
	encode    []float64
	transport []float64
	ack       []float64
}

func newFrameMetrics(surface string, record func(string)) *frameMetrics {
	return &frameMetrics{surface: surface, record: record, started: time.Now()}
}

func (m *frameMetrics) unchanged() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.skipped++
	m.mu.Unlock()
	m.flush(false)
}

// frameGuestTimes are the guest's own measurements of one frame: finding the
// window, laying it out, waiting for and capturing a frame, and encoding it.
type frameGuestTimes struct {
	target, layout, capture, encode float64
	cached                          bool
}

// frame records one delivered frame. request is the time from asking the
// guest until the whole image arrived; ack is the time from sending it to the
// page until acknowledged. Transport is what the guest did not account for.
func (m *frameMetrics) frame(request time.Duration, guest frameGuestTimes, size int, ack time.Duration) {
	if m == nil {
		return
	}
	requestMs := float64(request.Microseconds()) / 1000
	m.mu.Lock()
	m.frames++
	if guest.cached {
		m.cached++
	}
	m.bytes += int64(size)
	m.request = append(m.request, requestMs)
	m.target = append(m.target, guest.target)
	m.layout = append(m.layout, guest.layout)
	m.capture = append(m.capture, guest.capture)
	m.encode = append(m.encode, guest.encode)
	m.transport = append(m.transport, math.Max(0, requestMs-guest.target-guest.layout-guest.capture-guest.encode))
	m.ack = append(m.ack, float64(ack.Microseconds())/1000)
	m.mu.Unlock()
	m.flush(false)
}

// flush writes a summary once per window, or at the end of the stream.
func (m *frameMetrics) flush(final bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	elapsed := time.Since(m.started)
	if m.frames == 0 && m.skipped == 0 || !final && elapsed < frameMetricsWindow {
		m.mu.Unlock()
		return
	}
	message := m.summaryLocked(elapsed)
	m.started, m.frames, m.skipped, m.cached, m.bytes = time.Now(), 0, 0, 0, 0
	m.request, m.target, m.layout, m.capture, m.encode, m.transport, m.ack = nil, nil, nil, nil, nil, nil, nil
	m.mu.Unlock()
	if m.record != nil {
		m.record(message)
	}
}

func (m *frameMetrics) summaryLocked(elapsed time.Duration) string {
	seconds := math.Max(elapsed.Seconds(), 0.001)
	average := int64(0)
	if m.frames > 0 {
		average = m.bytes / int64(m.frames)
	}
	parts := []string{
		"surface=" + m.surface,
		fmt.Sprintf("seconds=%.1f", seconds),
		fmt.Sprintf("frames=%d", m.frames),
		fmt.Sprintf("fps=%.1f", float64(m.frames)/seconds),
		fmt.Sprintf("unchanged=%d", m.skipped),
		fmt.Sprintf("cached=%d", m.cached),
		fmt.Sprintf("bytes_avg=%d", average),
		fmt.Sprintf("mbps=%.1f", float64(m.bytes)*8/seconds/1e6),
	}
	for _, series := range []struct {
		name   string
		values []float64
	}{{"request", m.request}, {"target", m.target}, {"layout", m.layout}, {"capture", m.capture}, {"encode", m.encode}, {"transport", m.transport}, {"ack", m.ack}} {
		parts = append(parts, fmt.Sprintf("%s_ms=%s", series.name, percentiles(series.values)))
	}
	return strings.Join(parts, " ")
}

// percentiles formats p50/p95/max; "-" when there are no samples.
func percentiles(values []float64) string {
	if len(values) == 0 {
		return "-"
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	at := func(q float64) float64 { return sorted[int(math.Ceil(q*float64(len(sorted))))-1] }
	return fmt.Sprintf("%.0f/%.0f/%.0f", at(0.5), at(0.95), sorted[len(sorted)-1])
}

func (s *Server) recordFrameMetrics(message string) {
	if s.profile == "" {
		return
	}
	_ = diagnostics.New(s.profile).Record("info", "window-frames", message)
}

// inputMetrics summarizes the round trip of window input requests (pointer,
// keys, scrolling) on the ordered input channel. A nil value records nothing.
type inputMetrics struct {
	mu      sync.Mutex
	record  func(string)
	started time.Time
	latency map[string][]float64
}

func newInputMetrics(record func(string)) *inputMetrics {
	return &inputMetrics{record: record, started: time.Now(), latency: map[string][]float64{}}
}

func (m *inputMetrics) observe(action string, latency time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.latency[action] = append(m.latency[action], float64(latency.Microseconds())/1000)
	m.mu.Unlock()
	m.flush(false)
}

func (m *inputMetrics) flush(final bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	elapsed := time.Since(m.started)
	if len(m.latency) == 0 || !final && elapsed < frameMetricsWindow {
		m.mu.Unlock()
		return
	}
	actions := make([]string, 0, len(m.latency))
	for action := range m.latency {
		actions = append(actions, action)
	}
	slices.Sort(actions)
	parts := []string{fmt.Sprintf("seconds=%.1f", elapsed.Seconds())}
	for _, action := range actions {
		parts = append(parts, fmt.Sprintf("%s=%d:%s", action, len(m.latency[action]), percentiles(m.latency[action])))
	}
	m.latency, m.started = map[string][]float64{}, time.Now()
	m.mu.Unlock()
	if m.record != nil {
		m.record(strings.Join(parts, " "))
	}
}

func (s *Server) recordInputMetrics(message string) {
	if s.profile == "" {
		return
	}
	_ = diagnostics.New(s.profile).Record("info", "window-input", message)
}
