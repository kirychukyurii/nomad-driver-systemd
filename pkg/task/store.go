package task

import "sync"

// Store maps task IDs to the handlers a driver currently manages.
//
// All methods are safe for concurrent use by multiple goroutines. The zero value
// is not usable: create a Store with [NewStore].
//
// A Store holds handlers but does not own their lifecycle: removing a handler
// neither stops it nor stops its unit. [Store.Stop] is the sole exception, and it
// too leaves the units alone.
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

// Stop empties the Store and stops every handler it held, blocking until they
// have all finished.
//
// Handlers are stopped concurrently, so the slowest one to finish sets how long
// this takes rather than their sum. It does not stop their units, which outlive
// the handlers watching them. Handlers are removed before being stopped, so a
// concurrent lookup finds nothing rather than a handler on its way out, and a
// second Stop has nothing left to do - which is what keeps each handler stopped
// at most once, as [Handler.Stop] requires.
func (ts *Store) Stop() {
	ts.lock.Lock()

	handlers := make([]*Handler, 0, len(ts.store))
	for _, handler := range ts.store {
		handlers = append(handlers, handler)
	}

	ts.store = make(map[string]*Handler)
	ts.lock.Unlock()

	var wg sync.WaitGroup

	for _, handler := range handlers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			handler.Stop()
		}()
	}

	wg.Wait()
}
