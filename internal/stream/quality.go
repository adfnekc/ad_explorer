package stream

import (
	"sync"
)

// Adaptive quality.
//
// The rule is asymmetric on purpose: quality drops quickly when frames are
// being discarded and recovers slowly when they are not. Symmetric stepping
// oscillates — the encoder raises quality, the link saturates, frames drop, the
// encoder lowers it — and the operator sees the picture pulse between sharp and
// blurry for as long as the link is marginal.
//
// Safe for concurrent use: one goroutine encodes and reads Value, another
// handles the client's manual setting and the back-off decisions.
type Quality struct {
	mu sync.Mutex

	cur, min, max int
	step          int

	// Consecutive frames dropped before backing off, and consecutive frames
	// delivered before trying to recover. The second is much larger so the
	// recovery attempt happens well after the link has settled.
	dropLimit int
	goodLimit int

	dropped int
	good    int
}

// Quality defaults.
const (
	// DefaultDropLimit is roughly two thirds of a second of dropped frames at
	// 30fps.
	DefaultDropLimit = 20
	// DefaultGoodLimit is about three seconds at 30fps.
	DefaultGoodLimit = 90
	// DefaultStep is how much one back-off removes.
	DefaultStep = 8
)

// NewQuality returns a quality governor that keeps Value between min and max,
// starting at start. Out-of-range arguments are clamped rather than rejected:
// the values come from a config file and a mistake there should not stop a
// session from starting.
func NewQuality(min, max, start int) *Quality {
	if min < 1 {
		min = 1
	}
	if max > 100 {
		max = 100
	}
	if max < min {
		max = min
	}
	if start < min {
		start = min
	}
	if start > max {
		start = max
	}
	return &Quality{
		cur:       start,
		min:       min,
		max:       max,
		step:      DefaultStep,
		dropLimit: DefaultDropLimit,
		goodLimit: DefaultGoodLimit,
	}
}

// Value is the quality to encode the next frame at.
func (q *Quality) Value() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cur
}

// Min and Max report the bounds this governor works within. They are fixed at
// construction, so they are read without the lock.
func (q *Quality) Min() int { return q.min }
func (q *Quality) Max() int { return q.max }

// Set applies a quality the operator chose by hand. It replaces the current
// value but not the bounds, so an automatic back-off can still protect a link
// that cannot carry what was asked for.
func (q *Quality) Set(v int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if v < q.min {
		v = q.min
	}
	if v > q.max {
		v = q.max
	}
	q.cur = v
	q.dropped, q.good = 0, 0
}

// OnDropped records a frame that had to be discarded because the client could
// not keep up.
func (q *Quality) OnDropped() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.good = 0
	q.dropped++
	if q.dropped < q.dropLimit {
		return
	}
	q.dropped = 0
	// Recover by less than we back off: climbing back should take longer than
	// falling, or the picture flickers at the edge of what the link carries.
	q.cur = maxInt(q.min, q.cur-q.step)
}

// OnSent records a frame that went out.
func (q *Quality) OnSent() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropped = 0
	q.good++
	if q.good < q.goodLimit {
		return
	}
	q.good = 0
	q.cur = minInt(q.max, q.cur+q.step/2)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
