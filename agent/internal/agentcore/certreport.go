package agentcore

import (
	"sync"
	"time"

	"github.com/everwas/everwas/agent/internal/netcert"
)

// CertReport is what the device is ACTUALLY holding, published by the netcert
// loop and read by the heartbeat.
//
// The distinction it exists to carry: the server knows what it last issued,
// which is not the same as what the machine has. They diverge whenever a
// renewal half-failed, whenever a machine is restored from a backup image or
// cloned from a template, and whenever material is deleted by hand. Every one
// of those is invisible today, and each one shows up later as an
// authentication failure nobody can explain.
//
// Two goroutines, so it is guarded: the netcert loop writes every check, the
// heartbeat reads every thirty seconds.
type CertReport struct {
	mu       sync.RWMutex
	serial   string
	notAfter time.Time
}

// Set records what the device holds. A nil material means it holds nothing,
// which is itself worth reporting rather than leaving the previous value in
// place: a device whose certificate was deleted should stop claiming it.
func (c *CertReport) Set(m *netcert.Material) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m == nil {
		c.serial, c.notAfter = "", time.Time{}
		return
	}
	c.serial, c.notAfter = m.Serial, m.NotAfter
}

// Get returns the serial and expiry to report, empty if the device holds
// nothing or has not looked yet.
func (c *CertReport) Get() (serial string, notAfter time.Time) {
	if c == nil {
		return "", time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serial, c.notAfter
}
