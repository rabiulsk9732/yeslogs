package store

import (
	"context"
	"strings"
	"sync"
	"time"
)

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu       sync.Mutex
	isps     map[uint32]ISP
	users    map[string]User // by email
	devices  map[int64]Device
	agents   map[string]Agent // by token hash
	nextISP  uint32
	nextUser int64
	nextDev  int64
	nextAgt  int64
}

// NewMem returns an empty in-memory Store.
func NewMem() *MemStore {
	return &MemStore{
		isps:    map[uint32]ISP{},
		users:   map[string]User{},
		devices: map[int64]Device{},
		agents:  map[string]Agent{},
	}
}

func (m *MemStore) Migrate(context.Context) error { return nil }
func (m *MemStore) Close() error                  { return nil }

func (m *MemStore) CreateISP(_ context.Context, name string) (ISP, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.isps {
		if v.Name == name {
			return ISP{}, ErrDuplicate
		}
	}
	m.nextISP++
	v := ISP{ID: m.nextISP, Name: name, Enabled: true, CreatedAt: time.Now().UTC()}
	m.isps[v.ID] = v
	return v, nil
}

func (m *MemStore) GetISP(_ context.Context, id uint32) (ISP, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.isps[id]
	if !ok {
		return ISP{}, ErrNotFound
	}
	return v, nil
}

func (m *MemStore) ListISPs(context.Context) ([]ISP, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ISP, 0, len(m.isps))
	for _, v := range m.isps {
		out = append(out, v)
	}
	return out, nil
}

func (m *MemStore) SetISPEnabled(_ context.Context, id uint32, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.isps[id]
	if !ok {
		return ErrNotFound
	}
	v.Enabled = enabled
	m.isps[id] = v
	return nil
}

func (m *MemStore) CreateUser(_ context.Context, u User) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[strings.ToLower(u.Email)]; ok {
		return User{}, ErrDuplicate
	}
	m.nextUser++
	u.ID = m.nextUser
	u.CreatedAt = time.Now().UTC()
	m.users[strings.ToLower(u.Email)] = u
	return u, nil
}

func (m *MemStore) GetUserByEmail(_ context.Context, email string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[strings.ToLower(email)]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (m *MemStore) CountUsers(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users), nil
}

func (m *MemStore) CreateDevice(_ context.Context, d Device) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.devices {
		if v.ExporterIP == d.ExporterIP {
			return Device{}, ErrDuplicate
		}
	}
	m.nextDev++
	d.ID = m.nextDev
	d.UpdatedAt = time.Now().UTC()
	m.devices[d.ID] = d
	return d, nil
}

func (m *MemStore) GetDevice(_ context.Context, id int64) (Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[id]
	if !ok {
		return Device{}, ErrNotFound
	}
	return d, nil
}

func (m *MemStore) ListDevices(_ context.Context, ispID uint32) ([]Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Device, 0)
	for _, d := range m.devices {
		if ispID == 0 || d.ISPID == ispID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *MemStore) UpdateDevice(_ context.Context, d Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.devices[d.ID]; !ok {
		return ErrNotFound
	}
	for id, v := range m.devices {
		if id != d.ID && v.ExporterIP == d.ExporterIP {
			return ErrDuplicate
		}
	}
	d.UpdatedAt = time.Now().UTC()
	m.devices[d.ID] = d
	return nil
}

func (m *MemStore) DeleteDevice(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.devices[id]; !ok {
		return ErrNotFound
	}
	delete(m.devices, id)
	return nil
}

func (m *MemStore) CreateAgent(_ context.Context, name, tokenHash string) (Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agents[tokenHash]; ok {
		return Agent{}, ErrDuplicate
	}
	m.nextAgt++
	a := Agent{ID: m.nextAgt, Name: name, TokenHash: tokenHash, CreatedAt: time.Now().UTC()}
	m.agents[tokenHash] = a
	return a, nil
}

func (m *MemStore) GetAgentByToken(_ context.Context, tokenHash string) (Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[tokenHash]
	if !ok {
		return Agent{}, ErrNotFound
	}
	return a, nil
}

func (m *MemStore) TouchAgent(_ context.Context, id int64, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, a := range m.agents {
		if a.ID == id {
			a.LastSeen = t
			m.agents[k] = a
			return nil
		}
	}
	return ErrNotFound
}
