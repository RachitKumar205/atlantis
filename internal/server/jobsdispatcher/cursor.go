// Round-robin cursor for session selection. Approximate fairness:
// the counter increments monotonically, callers mod by the eligible
// session count. No persistence; restart resets to zero, which is
// fine because steady-state queue load amortizes any startup-window
// asymmetry across all sessions.

package jobsdispatcher

import "sync/atomic"

type cursor struct {
	n atomic.Uint32
}

func newCursor() *cursor { return &cursor{} }

func (c *cursor) next() uint32 { return c.n.Add(1) - 1 }
