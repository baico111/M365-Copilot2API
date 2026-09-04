package web

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestConversationGateSerializesSameConversation(t *testing.T) {
	g := newConversationGate()
	key := "acc1|conv1"

	release, ok := g.acquire(context.Background(), key, time.Second)
	if !ok {
		t.Fatal("first acquire must succeed")
	}
	other, ok2 := g.acquire(context.Background(), key, 80*time.Millisecond)
	if ok2 {
		t.Fatal("second acquire must not succeed while held")
	}
	if other == nil {
		t.Fatal("failed acquire must still return a no-op release (defer-callable)")
	}
	other()
	release()

	release, ok = g.acquire(context.Background(), key, time.Second)
	if !ok {
		t.Fatal("acquire must succeed after release")
	}
	release()
}

func TestConversationGateReleaseWakesWaiter(t *testing.T) {
	g := newConversationGate()
	key := "acc1|conv2"
	release, ok := g.acquire(context.Background(), key, time.Second)
	if !ok {
		t.Fatal("first acquire failed")
	}
	done := make(chan bool, 1)
	go func() {
		r2, ok2 := g.acquire(context.Background(), key, 2*time.Second)
		if ok2 {
			r2()
		}
		done <- ok2
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("waiter acquired prematurely")
	default:
	}
	release()
	select {
	case got := <-done:
		if !got {
			t.Fatal("waiter did not take over after release")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never woke up")
	}
}

func TestConversationGateConcurrentMutualExclusion(t *testing.T) {
	g := newConversationGate()
	key := "acc1|conv3"
	var mu sync.Mutex
	held := 0
	maxHeld := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 50; attempt++ {
				release, ok := g.acquire(context.Background(), key, time.Second)
				if !ok {
					continue
				}
				mu.Lock()
				held++
				if held > maxHeld {
					maxHeld = held
				}
				mu.Unlock()
				time.Sleep(2 * time.Millisecond)
				mu.Lock()
				held--
				mu.Unlock()
				release()
				return
			}
			t.Error("goroutine never acquired the gate")
		}()
	}
	wg.Wait()
	if maxHeld != 1 {
		t.Fatalf("gate allowed %d simultaneous holders", maxHeld)
	}
}

func TestConversationGateEmptyKeyIsNoop(t *testing.T) {
	g := newConversationGate()
	release, ok := g.acquire(context.Background(), "", time.Second)
	if !ok {
		t.Fatal("empty key must always acquire")
	}
	release()
	release() // idempotent
}

func TestConversationGateContextCancel(t *testing.T) {
	g := newConversationGate()
	key := "acc1|conv4"
	release, ok := g.acquire(context.Background(), key, time.Second)
	if !ok {
		t.Fatal("initial acquire failed")
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok = g.acquire(ctx, key, time.Second)
	if ok {
		t.Fatal("cancelled ctx must not acquire a held key")
	}
}
