package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// FileStore 基于 JSON 文件的持久化实现。
// 重启后 Load/List 可恢复订阅（含 PendingChange），
// 未完成的变更按下发时的语义由 Advance 继续推进。
type FileStore struct {
	mu   sync.Mutex
	dir  string
	subs map[string]*Subscription
}

func NewFileStore(dir string) (*FileStore, error) {
	fs := &FileStore{dir: dir, subs: make(map[string]*Subscription)}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "subscriptions.json"))
	if err == nil {
		var subs []*Subscription
		if err := json.Unmarshal(data, &subs); err != nil {
			return nil, err
		}
		for _, s := range subs {
			fs.subs[s.ID] = s
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return fs, nil
}

func (s *FileStore) flush() error {
	subs := make([]*Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		subs = append(subs, sub)
	}
	data, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "subscriptions.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "subscriptions.json"))
}

func (s *FileStore) Save(sub *Subscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sub
	s.subs[sub.ID] = &cp
	return s.flush()
}

func (s *FileStore) Load(id string) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *sub
	return &cp, nil
}

func (s *FileStore) List() ([]*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		cp := *sub
		out = append(out, &cp)
	}
	return out, nil
}
