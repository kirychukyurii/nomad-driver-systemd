// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"

	"github.com/kirychuk/nomad-systemd-driver-plugin/pkg/logx"
)

// pprofReadHeaderTimeout bounds how long the pprof server waits for a client to
// send its request headers.
const pprofReadHeaderTimeout = 5 * time.Second

// pprofShutdownTimeout bounds how long stopping the pprof server waits for
// in-flight profile requests to finish before the listener is closed anyway.
//
// A CPU profile or trace runs far longer than this; the point is to release the
// address promptly on a config reload, not to let a profile complete.
const pprofShutdownTimeout = 2 * time.Second

// pprofServer serves the net/http/pprof endpoints on a single address.
//
// The zero value is not usable: create one with [startPprof]. A server stops
// when its context is cancelled or [pprofServer.stop] is called, whichever comes
// first; stop may be called any number of times.
type pprofServer struct {
	// addr is the configured listen address, as written in the driver
	// configuration rather than as resolved by the listener. It is what a later
	// configuration is compared against.
	addr string

	// ln is retained so that stop can close it itself. http.Server.Shutdown also
	// closes the listener, but it does so from whichever goroutine reaches it
	// first, and a Serve that has not started yet closes it only once it does -
	// after which the address is briefly still bound.
	ln net.Listener

	srv      *http.Server
	stopOnce sync.Once
}

// configurePprof brings the driver's pprof server in line with addr: it starts
// one if addr is non-empty, stops the running one if addr is empty, and restarts
// it if the address changed.
//
// An unchanged address is a no-op, so a config reload that does not touch
// pprof_addr leaves the server and its address binding alone.
func (d *Driver) configurePprof(addr string) error {
	d.pprofLock.Lock()
	defer d.pprofLock.Unlock()

	if d.pprof != nil {
		if d.pprof.addr == addr {
			return nil
		}

		// Stop before binding the new address: the two may be the same port on
		// different hosts, or the same address written differently.
		d.pprof.stop()
		d.pprof = nil
	}

	if addr == "" {
		return nil
	}

	p, err := startPprof(d.ctx, addr, d.logger)
	if err != nil {
		return err
	}

	d.pprof = p

	return nil
}

// startPprof binds addr and serves the pprof endpoints on it until ctx is
// cancelled, returning an error if the address cannot be bound.
//
// Binding happens before returning so that an unusable address is reported as a
// configuration error; only serving is left to a goroutine.
func startPprof(ctx context.Context, addr string, logger logx.Logger) (*pprofServer, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	// A dedicated mux keeps the endpoints off http.DefaultServeMux, which is
	// process-global state shared with anything else that registers on it.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	p := &pprofServer{
		addr: addr,
		ln:   ln,
		srv: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: pprofReadHeaderTimeout,
		},
	}

	logger = logger.Named("pprof")

	if host, _, splitErr := net.SplitHostPort(ln.Addr().String()); splitErr == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
			logger.Warn("pprof is listening on a non-loopback address; its profiles expose the memory of a process that talks to systemd as root",
				logx.String("addr", ln.Addr().String()))
		}
	}

	logger.Info("pprof listening", logx.String("addr", ln.Addr().String()))

	go func() {
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof server stopped", logx.Err(err))
		}
	}()

	go func() {
		<-ctx.Done()
		p.stop()
	}()

	return p, nil
}

// stop shuts the server down; once it returns, the address is free to rebind.
func (p *pprofServer) stop() {
	p.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), pprofShutdownTimeout)
		defer cancel()

		_ = p.srv.Shutdown(ctx)
		_ = p.ln.Close()
	})
}
