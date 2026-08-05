package task

import (
	"sync"
	"testing"

	"github.com/hashicorp/nomad/plugins/drivers"
)

func TestStore_SetGetDelete(t *testing.T) {
	store := NewStore()

	if _, ok := store.Get("a"); ok {
		t.Fatalf("expected no handler for unknown id")
	}

	h := &Handler{taskID: "a", Unit: "a.service"}
	store.Set("a", h)

	got, ok := store.Get("a")
	if !ok {
		t.Fatalf("expected handler to be found after Set")
	}

	if got != h {
		t.Fatalf("expected Get to return the same handler instance")
	}

	store.Delete("a")

	if _, ok := store.Get("a"); ok {
		t.Fatalf("expected handler to be gone after Delete")
	}
}

func TestStore_Stop(t *testing.T) {
	store := NewStore()

	// An empty Store has nothing to stop and must neither block nor panic.
	store.Stop()

	handlers := map[string]*Handler{
		"a": newTestHandler(t, &fakeUnits{}, drivers.TaskStateRunning),
		"b": newTestHandler(t, &fakeUnits{}, drivers.TaskStateRunning),
	}

	for id, handler := range handlers {
		store.Set(id, handler)
	}

	store.Stop()

	for id, handler := range handlers {
		if _, ok := store.Get(id); ok {
			t.Fatalf("Stop left handler %q in the store", id)
		}

		if handler.ctx.Err() == nil {
			t.Fatalf("Stop left handler %q running", id)
		}
	}

	// Nothing is left to stop, so this must not stop any handler a second time.
	store.Stop()
}

func TestStore_ConcurrentAccess(t *testing.T) {
	store := NewStore()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			id := "task"
			store.Set(id, &Handler{taskID: id})
			store.Get(id)
			store.Delete(id)
		}(i)
	}

	wg.Wait()
}
