package plugin

import (
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// pprofURL returns the /debug/pprof/ URL of a running server.
func pprofURL(t *testing.T, p *pprofServer) string {
	t.Helper()

	if p == nil {
		t.Fatal("pprof server is not running")
	}

	return "http://" + p.addr + "/debug/pprof/cmdline"
}

// freeLoopbackAddr returns a loopback address that is free at the moment it
// returns, for tests that need a fixed address rather than port 0.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}

	return addr
}

func get(t *testing.T, url string) (*http.Response, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}

	return client.Do(req)
}

func TestConfigurePprof_Disabled(t *testing.T) {
	d := newTestDriver()
	defer d.signalShutdown()

	if err := d.configurePprof(""); err != nil {
		t.Fatalf("configurePprof(\"\") = %v, want nil", err)
	}

	if d.pprof != nil {
		t.Fatal("empty pprof_addr started a server")
	}
}

func TestConfigurePprof_ServesProfiles(t *testing.T) {
	d := newTestDriver()
	defer d.signalShutdown()

	if err := d.configurePprof(freeLoopbackAddr(t)); err != nil {
		t.Fatalf("configurePprof = %v, want nil", err)
	}

	resp, err := get(t, pprofURL(t, d.pprof))
	if err != nil {
		t.Fatalf("request pprof endpoint: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pprof endpoint status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestConfigurePprof_UnchangedAddrKeepsServer(t *testing.T) {
	d := newTestDriver()
	defer d.signalShutdown()

	addr := freeLoopbackAddr(t)

	if err := d.configurePprof(addr); err != nil {
		t.Fatalf("first configurePprof = %v, want nil", err)
	}

	first := d.pprof

	// A reload with the same address must not rebind it; if it did, the second
	// call would fail with "address already in use" or replace the server.
	if err := d.configurePprof(addr); err != nil {
		t.Fatalf("second configurePprof = %v, want nil", err)
	}

	if d.pprof != first {
		t.Fatal("unchanged pprof_addr restarted the server")
	}
}

func TestConfigurePprof_ChangedAddrRebinds(t *testing.T) {
	d := newTestDriver()
	defer d.signalShutdown()

	oldAddr := freeLoopbackAddr(t)

	if err := d.configurePprof(oldAddr); err != nil {
		t.Fatalf("first configurePprof = %v, want nil", err)
	}

	if err := d.configurePprof(freeLoopbackAddr(t)); err != nil {
		t.Fatalf("second configurePprof = %v, want nil", err)
	}

	if d.pprof == nil || d.pprof.addr == oldAddr {
		t.Fatal("changed pprof_addr did not move the server")
	}

	// The old address must be free again, or a reload back to it would fail.
	ln, err := net.Listen("tcp", oldAddr)
	if err != nil {
		t.Fatalf("old address still bound: %v", err)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
}

func TestConfigurePprof_EmptyAddrStopsServer(t *testing.T) {
	d := newTestDriver()
	defer d.signalShutdown()

	addr := freeLoopbackAddr(t)

	if err := d.configurePprof(addr); err != nil {
		t.Fatalf("configurePprof = %v, want nil", err)
	}

	if err := d.configurePprof(""); err != nil {
		t.Fatalf("configurePprof(\"\") = %v, want nil", err)
	}

	if d.pprof != nil {
		t.Fatal("empty pprof_addr left a server running")
	}

	resp, err := get(t, "http://"+addr+"/debug/pprof/cmdline")
	if err == nil {
		_ = resp.Body.Close()

		t.Fatal("stopped pprof server is still serving")
	}
}

func TestConfigurePprof_BadAddrIsConfigError(t *testing.T) {
	d := newTestDriver()
	defer d.signalShutdown()

	if err := d.configurePprof("127.0.0.1:not-a-port"); err == nil {
		t.Fatal("configurePprof accepted an unbindable address")
	}

	if d.pprof != nil {
		t.Fatal("failed configurePprof left a server behind")
	}
}

func TestConfigurePprof_ShutdownStopsServer(t *testing.T) {
	d := newTestDriver()

	addr := freeLoopbackAddr(t)

	if err := d.configurePprof(addr); err != nil {
		t.Fatalf("configurePprof = %v, want nil", err)
	}

	d.signalShutdown()

	// The listener closes asynchronously once the driver context is canceled.
	deadline := time.Now().Add(2 * time.Second)

	for {
		resp, err := get(t, "http://"+addr+"/debug/pprof/cmdline")
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatalf("request timed out rather than being refused: %v", err)
			}

			return
		}

		_ = resp.Body.Close()

		if time.Now().After(deadline) {
			t.Fatal("pprof server still serving after driver shutdown")
		}

		time.Sleep(10 * time.Millisecond)
	}
}
