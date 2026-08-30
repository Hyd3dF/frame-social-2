package api

import "sync"

type memberCache struct {
	mu   sync.RWMutex
	data map[string][]string
}

func newMemberCache() *memberCache {
	return &memberCache{data: make(map[string][]string)}
}

func (c *memberCache) Get(conversation string) ([]string, bool) {
	c.mu.RLock()
	v, ok := c.data[conversation]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	out := make([]string, len(v))
	copy(out, v)
	return out, true
}

func (c *memberCache) Set(conversation string, members []string) {
	cp := make([]string, len(members))
	copy(cp, members)
	c.mu.Lock()
	c.data[conversation] = cp
	c.mu.Unlock()
}

func (c *memberCache) Delete(conversation string) {
	c.mu.Lock()
	delete(c.data, conversation)
	c.mu.Unlock()
}

func (c *memberCache) Clear() {
	c.mu.Lock()
	c.data = make(map[string][]string)
	c.mu.Unlock()
}

func (c *memberCache) IsMember(conversation, account string) (bool, bool) {
	c.mu.RLock()
	members, ok := c.data[conversation]
	c.mu.RUnlock()
	if !ok {
		return false, false
	}
	for _, m := range members {
		if m == account {
			return true, true
		}
	}
	return false, true
}

type pendingStore struct {
	mu   sync.Mutex
	data map[string][]messageView
}

func newPendingStore() *pendingStore {
	return &pendingStore{data: make(map[string][]messageView)}
}

func (s *pendingStore) Append(conversation string, msg messageView) {
	s.mu.Lock()
	s.data[conversation] = append(s.data[conversation], msg)
	if len(s.data[conversation]) > 1000 {
		s.data[conversation] = s.data[conversation][len(s.data[conversation])-1000:]
	}
	s.mu.Unlock()
}

func (s *pendingStore) List(conversation string) []messageView {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data[conversation]
	out := make([]messageView, len(list))
	copy(out, list)
	return out
}

func (s *pendingStore) Remove(conversation string, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data[conversation]
	for i, m := range list {
		if m.ID == id {
			s.data[conversation] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(s.data[conversation]) == 0 {
		delete(s.data, conversation)
	}
}
