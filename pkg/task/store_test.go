package task

import (
	"sync"
	"testing"
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

func TestStore_Handlers(t *testing.T) {
	store := NewStore()

	if got := store.Handlers(); len(got) != 0 {
		t.Fatalf("Handlers() on an empty store returned %d handlers, want 0", len(got))
	}

	a := &Handler{taskID: "a", Unit: "a.service"}
	b := &Handler{taskID: "b", Unit: "b.service"}

	store.Set("a", a)
	store.Set("b", b)

	got := store.Handlers()
	if len(got) != 2 {
		t.Fatalf("Handlers() returned %d handlers, want 2", len(got))
	}

	seen := make(map[*Handler]bool, len(got))
	for _, h := range got {
		seen[h] = true
	}

	if !seen[a] || !seen[b] {
		t.Fatalf("Handlers() did not return both stored handlers")
	}

	// The snapshot is the caller's own: later mutation must not reach it.
	store.Delete("a")

	if len(got) != 2 {
		t.Fatalf("Delete changed an already-returned snapshot: len = %d, want 2", len(got))
	}

	if remaining := store.Handlers(); len(remaining) != 1 || remaining[0] != b {
		t.Fatalf("Handlers() after Delete = %v, want only the remaining handler", remaining)
	}
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
