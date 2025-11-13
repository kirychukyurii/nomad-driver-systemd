package task

import "sync"

// Store provides thread-safe storage for task handlers
type Store struct {
	store map[string]*Handler
	lock  sync.RWMutex
}

// NewStore creates a new task handler store
func NewStore() *Store {
	return &Store{
		store: make(map[string]*Handler),
	}
}

// Set stores a task handler
func (ts *Store) Set(id string, handler *Handler) {
	ts.lock.Lock()
	defer ts.lock.Unlock()
	ts.store[id] = handler
}

// Get retrieves a task handler
func (ts *Store) Get(id string) (*Handler, bool) {
	ts.lock.RLock()
	defer ts.lock.RUnlock()
	handler, ok := ts.store[id]

	return handler, ok
}

// Delete removes a task handler
func (ts *Store) Delete(id string) {
	ts.lock.Lock()
	defer ts.lock.Unlock()
	delete(ts.store, id)
}

// GetAll returns all task handlers
func (ts *Store) GetAll() []*Handler {
	ts.lock.RLock()
	defer ts.lock.RUnlock()

	handlers := make([]*Handler, 0, len(ts.store))
	for _, handler := range ts.store {
		handlers = append(handlers, handler)
	}

	return handlers
}
