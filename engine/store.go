package engine

import "sync"

// Store 持久化接口：重启后未完成的变更与订阅状态可恢复。
type Store interface {
	Save(sub *Subscription) error
	Load(id string) (*Subscription, error)
	List() ([]*Subscription, error)
}

// MemoryStore 内存实现。
type MemoryStore struct {
	mu   sync.Mutex
	subs map[string]*Subscription
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{subs: make(map[string]*Subscription)}
}

func (s *MemoryStore) Save(sub *Subscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sub
	s.subs[sub.ID] = &cp
	return nil
}

func (s *MemoryStore) Load(id string) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *sub
	return &cp, nil
}

func (s *MemoryStore) List() ([]*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		cp := *sub
		out = append(out, &cp)
	}
	return out, nil
}
