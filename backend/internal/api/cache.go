package api

import (
	"sync"
	"time"
)

type memberCache struct {
	mu   sync.RWMutex
	data map[string][]string
}

const maxCachedConversations = 10000

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
	if _, exists := c.data[conversation]; !exists && len(c.data) >= maxCachedConversations {
		for id := range c.data {
			delete(c.data, id)
			break
		}
	}
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
	mu     sync.Mutex
	data   map[string][]messageView
	hidden map[string]map[string]struct{}
	limit  int
	total  int
}

func newPendingStore() *pendingStore {
	return &pendingStore{data: make(map[string][]messageView), hidden: make(map[string]map[string]struct{}), limit: 10000}
}

func (s *pendingStore) TryAppend(conversation string, msg messageView) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= s.limit {
		return false
	}
	s.data[conversation] = append(s.data[conversation], msg)
	s.total++
	return true
}

func (s *pendingStore) Append(conversation string, msg messageView) {
	_ = s.TryAppend(conversation, msg)
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
			s.total--
			break
		}
	}
	if len(s.data[conversation]) == 0 {
		delete(s.data, conversation)
	}
}

func (s *pendingStore) Find(id string) (messageView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, messages := range s.data {
		for _, message := range messages {
			if message.ID == id {
				return message, true
			}
		}
	}
	return messageView{}, false
}

func (s *pendingStore) Hide(account, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hidden[id] == nil {
		s.hidden[id] = make(map[string]struct{})
	}
	s.hidden[id][account] = struct{}{}
}

func (s *pendingStore) IsHidden(account, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, hidden := s.hidden[id][account]
	return hidden
}

func (s *pendingStore) Retract(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conversation, messages := range s.data {
		for index := range messages {
			if messages[index].ID == id {
				now := time.Now().UTC().Format(time.RFC3339Nano)
				messages[index].Body = nil
				messages[index].Deleted = true
				messages[index].DeletedAt = &now
				messages[index].Kind = "deleted"
				s.data[conversation] = messages
				return true
			}
		}
	}
	return false
}

func (s *pendingStore) DeleteEverywhere(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conversation, messages := range s.data {
		for index, message := range messages {
			if message.ID == id {
				s.data[conversation] = append(messages[:index], messages[index+1:]...)
				s.total--
				if len(s.data[conversation]) == 0 {
					delete(s.data, conversation)
				}
				return true
			}
		}
	}
	return false
}
