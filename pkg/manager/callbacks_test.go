package manager

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventEmitterEmitAfterCloseDoesNotBlockOrPanic(t *testing.T) {
	e := NewEventEmitterWithQueueSize(1)
	e.Close()

	if ok := e.Emit(Event{Type: EventDisconnected}); ok {
		t.Fatal("Emit after Close returned true, want false")
	}
}

func TestEventEmitterDropsWhenQueueFull(t *testing.T) {
	e := &EventEmitter{
		queue:   make(chan Event, 1),
		done:    make(chan struct{}),
		started: true,
	}

	if ok := e.Emit(Event{Type: EventConnected}); !ok {
		t.Fatal("first Emit returned false, want true")
	}
	if ok := e.Emit(Event{Type: EventDisconnected}); ok {
		t.Fatal("second Emit returned true with full queue, want false")
	}
	if dropped := e.Dropped(); dropped != 1 {
		t.Fatalf("Dropped=%d want 1", dropped)
	}
}

func TestEventEmitterLazyDoesNotLeakGoroutinesWhenUnused(t *testing.T) {
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
	before := runtime.NumGoroutine()

	// Create 64 unstarted managers + emitters without registering handlers or calling Close().
	managers := make([]*Manager, 64)
	for i := range managers {
		managers[i] = New(Config{}, NewNopLogger())
	}

	time.Sleep(10 * time.Millisecond)
	after := runtime.NumGoroutine()
	// Allow small runtime jitter (<=4), whereas eager worker spawning would leak 64+ goroutines!
	if leaked := after - before; leaked > 4 {
		t.Fatalf("New() without handlers spawned %d goroutines (before=%d after=%d), want lazy emitter", leaked, before, after)
	}

	// Registering a handler starts the worker lazily and delivers events; Close() stops it; reopen() revives it.
	e := managers[0].events
	var delivered atomic.Int32
	e.On(func(ev Event) {
		if ev.Type == EventConnected {
			delivered.Add(1)
		}
	})
	if !e.Emit(Event{Type: EventConnected}) {
		t.Fatal("Emit with registered handler returned false")
	}
	deadline := time.Now().Add(time.Second)
	for delivered.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if delivered.Load() != 1 {
		t.Fatalf("delivered=%d, want 1", delivered.Load())
	}

	e.Close()
	e.reopen(16)
	if !e.Emit(Event{Type: EventConnected}) {
		t.Fatal("Emit after reopen() returned false")
	}
	deadline = time.Now().Add(time.Second)
	for delivered.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if delivered.Load() != 2 {
		t.Fatalf("delivered after reopen=%d, want 2", delivered.Load())
	}
	e.Close()
}
