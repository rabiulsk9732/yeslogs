package pipeline

import (
	"sync"
	"time"
)

// DeviceSignals records what an exporter is still telling us when the rules drop
// everything it sends.
//
// The hard-coded no-translation rule means an exporter that logs traffic rather
// than NAT can store zero rows while remaining perfectly alive and busy. Every
// view that answers "is this device working?" reads stored rows — the liveness
// badge, the silence alert, the compliance grade — so without this the device
// would read as dead, the operator would go hunting a network fault, and the
// real problem (its export carries no post-NAT fields) would stay invisible.
//
// One instance is shared by every protocol pipeline, since a device is a device
// regardless of which listener its packets arrived on.
type DeviceSignals struct {
	mu sync.Mutex
	m  map[uint32]*devSig
}

type devSig struct {
	noNAT    uint64
	lastFlow time.Time
}

// DeviceSignal is one device's dropped-flow evidence.
type DeviceSignal struct {
	// NoNATDropped counts flows discarded for carrying no post-NAT address.
	NoNATDropped uint64
	// LastFlow is when such a flow last arrived — proof of life independent of
	// anything reaching storage.
	LastFlow time.Time
}

// NewDeviceSignals returns an empty tracker.
func NewDeviceSignals() *DeviceSignals {
	return &DeviceSignals{m: map[uint32]*devSig{}}
}

// noteNoNAT records one flow dropped for having no translation. Cheap enough to
// call per flow: one mutex and two field writes, on a path that already decided
// to throw the record away.
func (d *DeviceSignals) noteNoNAT(deviceID uint32, at time.Time) {
	if d == nil || deviceID == 0 {
		return
	}
	d.mu.Lock()
	s := d.m[deviceID]
	if s == nil {
		s = &devSig{}
		d.m[deviceID] = s
	}
	s.noNAT++
	s.lastFlow = at
	d.mu.Unlock()
}

// Snapshot returns a copy of the current per-device evidence.
func (d *DeviceSignals) Snapshot() map[uint32]DeviceSignal {
	out := map[uint32]DeviceSignal{}
	if d == nil {
		return out
	}
	d.mu.Lock()
	for id, s := range d.m {
		out[id] = DeviceSignal{NoNATDropped: s.noNAT, LastFlow: s.lastFlow}
	}
	d.mu.Unlock()
	return out
}

// SetDeviceSignals attaches the shared tracker. Optional: a pipeline with none
// simply records nothing.
func (p *Pipeline) SetDeviceSignals(d *DeviceSignals) { p.signals = d }
