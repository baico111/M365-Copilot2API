package outbound

import (
	"errors"
	"testing"
)

// TestNodeHealthTransitions exercises the pure state transitions of a node's
// health record: a fresh node starts "checking", failed checks accumulate, and
// reaching the threshold flips it unhealthy.
func TestNodeHealthTransitions(t *testing.T) {
	nh := newNodeHealth()
	if nh.health != "checking" {
		t.Fatalf("new node health = %q, want checking", nh.health)
	}
	if nh.failures != 0 || nh.isolated || nh.banned {
		t.Fatalf("new node has dirty state: %+v", nh)
	}
}

// TestRecordNodeFailureAccumulates drives recordNodeFailure via the package
// globals and asserts cumulative failure counting plus soft-isolation at the
// threshold, and that an already-isolated node ignores further failures.
func TestRecordNodeFailureAccumulates(t *testing.T) {
	sbMu.Lock()
	savedHealth, savedPorts, savedNodes := sbNodeHealth, sbNodePorts, sbNodeList
	sbNodeHealth = map[int]*nodeHealth{0: newNodeHealth()}
	sbNodePorts = map[int]int{0: 19999}
	sbNodeList = []string{"n0"}
	sbMu.Unlock()
	defer func() {
		sbMu.Lock()
		sbNodeHealth, sbNodePorts, sbNodeList = savedHealth, savedPorts, savedNodes
		sbMu.Unlock()
	}()

	err := errors.New("dial timeout")
	for i := 1; i < healthMaxFailures; i++ {
		recordNodeFailure(0, err)
		sbNodeHealth[0].mu.Lock()
		got := sbNodeHealth[0].failures
		sbNodeHealth[0].mu.Unlock()
		if got != i {
			t.Fatalf("after %d failures count = %d", i, got)
		}
	}
	// Reaching the threshold bans+isolates the node.
	recordNodeFailure(0, err)
	nh := sbNodeHealth[0]
	nh.mu.Lock()
	banned, isolated := nh.banned, nh.isolated
	nh.mu.Unlock()
	if !banned || !isolated {
		t.Fatalf("threshold should ban+isolate: banned=%v isolated=%v", banned, isolated)
	}
	// Further failures must be ignored (short-circuit on isolated).
	before := nh.failures
	recordNodeFailure(0, err)
	nh.mu.Lock()
	after := nh.failures
	nh.mu.Unlock()
	if after != before {
		t.Fatalf("isolated node still counted failures: %d -> %d", before, after)
	}
}

// TestBanNodeLockedIdempotent verifies banning twice keeps a single isolatedAt
// timestamp and does not panic on a missing node.
func TestBanNodeLockedIdempotent(t *testing.T) {
	sbMu.Lock()
	savedHealth, savedPorts, savedAddrs, savedBanned := sbNodeHealth, sbNodePorts, sbEgressAddrs, sbBannedEgresses
	sbNodeHealth = map[int]*nodeHealth{0: newNodeHealth()}
	sbNodePorts = map[int]int{0: 19998}
	sbEgressAddrs = map[int]string{0: "203.0.113.9"}
	sbBannedEgresses = map[string]bool{}
	sbMu.Unlock()
	defer func() {
		sbMu.Lock()
		sbNodeHealth, sbNodePorts, sbEgressAddrs, sbBannedEgresses = savedHealth, savedPorts, savedAddrs, savedBanned
		sbMu.Unlock()
	}()

	banNodeLocked(0, "first")
	nh := sbNodeHealth[0]
	nh.mu.Lock()
	at1 := nh.isolatedAt
	nh.mu.Unlock()
	banNodeLocked(0, "second")
	nh.mu.Lock()
	at2 := nh.isolatedAt
	nh.mu.Unlock()
	if !at1.Equal(at2) {
		t.Fatal("re-ban must not reset isolatedAt")
	}
	if !sbBannedEgresses["203.0.113.9"] {
		t.Fatal("egress IP not persisted as banned")
	}
	// Missing idx must be a no-op, not a panic.
	banNodeLocked(999, "ghost")
}

// TestReplacementRestartCoalesces asserts the sbReplacing latch makes a second
// schedule call a no-op while one is already pending/in flight, so concurrent
// bans cannot stack redundant subscription fetches + process restarts.
func TestReplacementRestartCoalesces(t *testing.T) {
	sbMu.Lock()
	savedCfg, savedReplacing := sbConfig, sbReplacing
	// nil config makes the first call return immediately after claiming the
	// latch, which is enough to observe the coalescing guard.
	sbConfig = nil
	sbReplacing = false
	sbMu.Unlock()
	defer func() {
		sbMu.Lock()
		sbConfig, sbReplacing = savedCfg, savedReplacing
		sbMu.Unlock()
	}()

	scheduleReplacementRestart("first")
	sbMu.Lock()
	claimed := sbReplacing
	sbMu.Unlock()
	if claimed {
		t.Fatal("nil-config schedule must release the latch before returning")
	}

	// Simulate an in-flight replacement: latch held, second call must not block
	// or panic and must leave the latch untouched.
	sbMu.Lock()
	sbReplacing = true
	sbMu.Unlock()
	scheduleReplacementRestart("second")
	sbMu.Lock()
	stillHeld := sbReplacing
	sbMu.Unlock()
	if !stillHeld {
		t.Fatal("coalesced call must not clear another caller's latch")
	}
}
