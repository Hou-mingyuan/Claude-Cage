package workbench

import (
	"strings"
	"testing"
	"time"
)

func TestFrameMetricsSummarizesWindowAndResets(t *testing.T) {
	var records []string
	metrics := newFrameMetrics("claude", func(message string) { records = append(records, message) })
	metrics.started = time.Now().Add(-frameMetricsWindow)
	metrics.unchanged()
	if len(records) != 1 || !strings.Contains(records[0], "frames=0") || !strings.Contains(records[0], "unchanged=1") || !strings.Contains(records[0], "request_ms=-") {
		t.Fatalf("records = %q", records)
	}
	metrics.started = time.Now().Add(-frameMetricsWindow)
	metrics.frame(80*time.Millisecond, frameGuestTimes{target: 20, layout: 10, capture: 10, encode: 30, cached: true}, 1000, 4*time.Millisecond)
	summary := records[len(records)-1]
	for _, want := range []string{"surface=claude", "frames=1", "bytes_avg=1000", "cached=1", "request_ms=80/80/80", "target_ms=20/20/20", "layout_ms=10/10/10", "capture_ms=10/10/10", "encode_ms=30/30/30", "transport_ms=10/10/10", "ack_ms=4/4/4"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q lacks %q", summary, want)
		}
	}
	metrics.flush(true)
	if len(records) != 2 {
		t.Fatalf("an empty window wrote a summary: %q", records)
	}
	var none *frameMetrics
	none.frame(time.Second, frameGuestTimes{}, 1, time.Second)
	none.unchanged()
	none.flush(true)
}

func TestInputMetricsGroupsActions(t *testing.T) {
	var records []string
	metrics := newInputMetrics(func(message string) { records = append(records, message) })
	metrics.observe("pointer", 20*time.Millisecond)
	metrics.observe("pointer", 40*time.Millisecond)
	metrics.observe("key", 5*time.Millisecond)
	if len(records) != 0 {
		t.Fatalf("summary before the window ended: %q", records)
	}
	metrics.flush(true)
	if len(records) != 1 || !strings.Contains(records[0], "key=1:5/5/5") || !strings.Contains(records[0], "pointer=2:20/40/40") {
		t.Fatalf("records = %q", records)
	}
	var none *inputMetrics
	none.observe("key", time.Second)
	none.flush(true)
}

func TestPercentiles(t *testing.T) {
	if got := percentiles([]float64{5, 1, 3, 2, 4, 6, 7, 8, 9, 100}); got != "5/100/100" {
		t.Fatalf("percentiles = %q", got)
	}
}
