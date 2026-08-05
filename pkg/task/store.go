package task

import "sync"

// Store maps task IDs to the handlers a driver currently manages.
//
// All methods are safe for concurrent use by multiple goroutines. The zero value
// is not usable: create a Store with [NewStore].
//
// A Store holds handlers but does not own their lifecycle: removing a handler
// neither stops it nor stops its unit.
type Store struct {
	store map[string]*Handler
	lock  sync.RWMutex
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		store: make(map[string]*Handler),
	}
}

// Set associates handler with id, replacing any handler already stored under it.
func (ts *Store) Set(id string, handler *Handler) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	ts.store[id] = handler
}

// Get returns the handler stored under id. The boolean reports whether one was
// found; when it is false the returned handler is nil.
func (ts *Store) Get(id string) (*Handler, bool) {
	ts.lock.RLock()
	defer ts.lock.RUnlock()

	handler, ok := ts.store[id]

	return handler, ok
}

// Delete removes any handler stored under id. Deleting an unknown id does
// nothing.
func (ts *Store) Delete(id string) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	delete(ts.store, id)
}
