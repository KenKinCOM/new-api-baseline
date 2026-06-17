package sms

import (
	"sync"
	"time"
)

type windowCounter struct {
	Count     int
	ExpiresAt time.Time
}

type MemoryLimiter struct {
	mutex sync.Mutex
	items map[string]windowCounter
}

func NewMemoryLimiter() *MemoryLimiter {
	return &MemoryLimiter{items: map[string]windowCounter{}}
}

func (l *MemoryLimiter) Allow(key string, limit int, windowSeconds int) bool {
	if limit <= 0 || windowSeconds <= 0 {
		return true
	}
	now := time.Now()
	l.mutex.Lock()
	defer l.mutex.Unlock()
	item, ok := l.items[key]
	if !ok || now.After(item.ExpiresAt) {
		l.items[key] = windowCounter{
			Count:     1,
			ExpiresAt: now.Add(time.Duration(windowSeconds) * time.Second),
		}
		return true
	}
	if item.Count >= limit {
		return false
	}
	item.Count++
	l.items[key] = item
	return true
}
