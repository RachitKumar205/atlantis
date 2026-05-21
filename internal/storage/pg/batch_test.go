package pg

import "testing"

func TestNewBatch_EmptyHasZeroLen(t *testing.T) {
	b := NewBatch()
	if b.Len() != 0 {
		t.Errorf("empty batch: got Len() = %d want 0", b.Len())
	}
}

func TestBatch_QueueGrowsLen(t *testing.T) {
	b := NewBatch()
	b.Queue("SELECT 1")
	b.Queue("SELECT 2", 42)
	b.Queue("INSERT INTO t (a) VALUES ($1)", "x")
	if b.Len() != 3 {
		t.Errorf("after 3 Queue calls: Len() = %d want 3", b.Len())
	}
}

// SendBatch / RunInTx require a real Postgres; their happy paths are
// covered by tests/integration (task #25). The unit tests above pin the
// shape of the wrapper API so generated code can rely on it.
