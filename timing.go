package moho

import (
	"math/bits"
	"sync/atomic"
	"time"
)

// TimingStats describes observed wall time, including scheduling pauses.
// Bucket i counts durations less than 2^i nanoseconds (bucket 0 includes 0).
// Quantiles derived from these buckets are upper bounds, not exact timings.
type TimingStats struct {
	Count      uint64
	TotalNS    uint64
	MaxNS      uint64
	OverBudget uint64
	Buckets    [64]uint64
}

type durationCounter struct {
	count, total, max, over atomic.Uint64
	buckets                 [64]atomic.Uint64
}

func (c *durationCounter) observe(d, budget time.Duration) {
	n := uint64(max(d, 0))
	c.buckets[min(bits.Len64(n), 63)].Add(1)
	c.total.Add(n)
	c.count.Add(1)
	for old := c.max.Load(); n > old; old = c.max.Load() {
		if c.max.CompareAndSwap(old, n) {
			break
		}
	}
	if d > budget {
		c.over.Add(1)
	}
}

func (c *durationCounter) reset() {
	c.count.Store(0)
	c.total.Store(0)
	c.max.Store(0)
	c.over.Store(0)
	for i := range c.buckets {
		c.buckets[i].Store(0)
	}
}

func (c *durationCounter) snapshot() TimingStats {
	s := TimingStats{Count: c.count.Load(), TotalNS: c.total.Load(), MaxNS: c.max.Load(), OverBudget: c.over.Load()}
	for i := range c.buckets {
		s.Buckets[i] = c.buckets[i].Load()
	}
	return s
}
