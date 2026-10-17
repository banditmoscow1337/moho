package audio

import (
	"testing"
	"time"
)

func TestDurationCounter(t *testing.T) {
	var c durationCounter
	for _, ns := range []int{0, 1, 7, 8, 20} {
		c.observe(time.Duration(ns), 8)
	}
	s := c.snapshot()
	if s.Count != 5 || s.TotalNS != 36 || s.MaxNS != 20 || s.OverBudget != 1 || s.Buckets[0] != 1 || s.Buckets[1] != 1 || s.Buckets[3] != 1 || s.Buckets[4] != 1 || s.Buckets[5] != 1 {
		t.Fatalf("bad histogram: %+v", s)
	}
	c.reset()
	if c.snapshot() != (TimingStats{}) {
		t.Fatal("reset retained observations")
	}
}

func TestTimingAndGenerationLifecycle(t *testing.T) {
	b := &testBackend{}
	e := NewEngineWithComponents(b, &testCodecs{})
	defer e.Cleanup()
	if err := e.EnableTiming(); err != nil {
		t.Fatal(err)
	}
	for generation := uint64(1); generation <= 2; generation++ {
		if err := e.Start(nil); err != nil {
			t.Fatal(err)
		}
		if err := e.EnableTiming(); err == nil {
			t.Fatal("configuration changed after initialization")
		}
		if s := e.Stats(); s.Generation != generation || !s.Running || !s.TimingEnabled || s.CallbackTiming.Count != 0 {
			t.Fatalf("start stats: %+v", s)
		}
		b.callback(make([]byte, frameBytes), make([]byte, frameBytes), FrameSizeSamples)
		e.Stop()
		if s := e.Stats(); s.Running || s.CallbackTiming.Count != 1 {
			t.Fatalf("stop stats: %+v", s)
		}
	}
}
