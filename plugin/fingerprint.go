// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/structs"
)

// fingerprintPeriod is the interval at which the driver will send fingerprint responses
const fingerprintPeriod = 30 * time.Second

// Fingerprint is called by the client when the plugin is started.
// It allows the driver to indicate its health to the client.
// The channel returned should immediately send an initial Fingerprint,
// then send periodic updates at an interval that is appropriate for the driver
// until the context is canceled.
func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)

	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-timer.C:
			timer.Reset(fingerprintPeriod)

			// Guarded so a caller that stopped listening cannot leak this
			// goroutine.
			select {
			case ch <- d.buildFingerprint():
			case <-ctx.Done():
				return
			case <-d.ctx.Done():
				return
			}
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	fp := &drivers.Fingerprint{
		Attributes:        make(map[string]*structs.Attribute),
		Health:            drivers.HealthStateHealthy,
		HealthDescription: drivers.DriverHealthy,
	}

	mgr, err := d.getManager()
	if err != nil {
		fp.Health = drivers.HealthStateUndetected
		fp.HealthDescription = "waiting for driver initialization"

		return fp
	}

	if !mgr.Healthy() {
		fp.Health = drivers.HealthStateUnhealthy
		fp.HealthDescription = "systemd is not available"

		return fp
	}

	fp.Attributes["driver.systemd"] = structs.NewBoolAttribute(true)
	fp.Attributes["driver.systemd.version"] = structs.NewStringAttribute(pluginVersion)
	fp.Attributes["driver.systemd.logs"] = structs.NewBoolAttribute(true)
	fp.Attributes["driver.systemd.signals"] = structs.NewBoolAttribute(false)

	// On a cgroup v1 or hybrid host tasks still run, but TaskStats reports zeros.
	fp.Attributes["driver.systemd.stats"] = structs.NewBoolAttribute(mgr.CgroupV2Available())

	return fp
}
