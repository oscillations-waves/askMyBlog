package main

import (
	"container/list"
	"sync"
	"time"
)

type cachedAnswer struct {
	question string
	text     string
	sources  []string
	expires  time.Time
}

// answerCache holds only completed answers. Its lifetime matches the immutable
// index loaded by this server process; restarts invalidate it automatically.
type answerCache struct {
	mu       sync.Mutex
	items    map[string]*list.Element
	order    *list.List
	capacity int
	ttl      time.Duration
}

func newAnswerCache(capacity int, ttl time.Duration) *answerCache {
	return &answerCache{items: make(map[string]*list.Element), order: list.New(), capacity: capacity, ttl: ttl}
}

func (c *answerCache) get(question string) (cachedAnswer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[question]
	if !ok {
		return cachedAnswer{}, false
	}
	answer := e.Value.(cachedAnswer)
	if time.Now().After(answer.expires) {
		c.order.Remove(e)
		delete(c.items, question)
		return cachedAnswer{}, false
	}
	c.order.MoveToFront(e)
	return answer, true
}

func (c *answerCache) put(question, text string, sources []string) {
	if c.capacity <= 0 || c.ttl <= 0 || len(text) > 64*1024 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[question]; ok {
		c.order.Remove(e)
		delete(c.items, question)
	}
	answer := cachedAnswer{question: question, text: text, sources: append([]string{}, sources...), expires: time.Now().Add(c.ttl)}
	c.items[question] = c.order.PushFront(answer)
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		delete(c.items, oldest.Value.(cachedAnswer).question)
		c.order.Remove(oldest)
	}
}
