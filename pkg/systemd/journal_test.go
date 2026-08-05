package systemd

import (
	"strings"
	"testing"
)

func TestStreamLogs_RefusedAfterStop(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	register(sm, "app.service")

	sm.unitsLock.Lock()
	sm.stopping = true
	sm.unitsLock.Unlock()

	if err := sm.StreamLogs("app.service", make(chan *LogEntry)); err == nil {
		t.Fatalf("expected StreamLogs to refuse work after manager stop")
	}
}

// TestStreamLogs_RefusedForUnregisteredUnit covers the ordering that used to
// leak a journal reader for the rest of the process's life: callers start
// StreamLogs from a detached goroutine, so a task destroyed right after being
// started can have its UnregisterUnit run first. Registering a cancel func at
// that point would leave nobody to call it, and the reader would block forever
// writing to the dead handler's LogCh.
//
// This exercises the pre-open check; the identical re-check after the journal
// is opened is what actually closes the race, but it can only be reached on a
// host with a real journal, which the test host is not.
func TestStreamLogs_RefusedForUnregisteredUnit(t *testing.T) {
	sm := newTestManager(t, &fakeDbusConn{connected: true})

	// Deliberately not registered: no RegisterUnit call, matching the state
	// left behind by UnregisterUnit.
	err := sm.StreamLogs("app.service", make(chan *LogEntry))
	if err == nil {
		t.Fatalf("expected StreamLogs to refuse an unregistered unit")
	}

	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error should say the unit is unregistered, got: %v", err)
	}

	sm.unitsLock.RLock()
	_, stored := sm.units["app.service"]
	sm.unitsLock.RUnlock()

	if stored {
		t.Errorf("a refused StreamLogs must not leave a cancel func behind")
	}
}
