package crm

import (
	"container/list"
	"fmt"
	"sync"
	"time"
)

type cacheEntry struct {
	key     string
	result  LookupResult
	expires time.Time
}

// Cache is a thread-safe LRU cache with per-item TTL.
type Cache struct {
	mu        sync.RWMutex
	capacity  int
	hitTTL    time.Duration
	missTTL   time.Duration
	items     map[string]*list.Element
	evictList *list.List
}

// NewCache creates an LRU Cache.
func NewCache(capacity int, hitTTL, missTTL time.Duration) *Cache {
	if capacity <= 0 {
		capacity = 50000
	}
	if hitTTL <= 0 {
		hitTTL = 15 * time.Minute
	}
	if missTTL <= 0 {
		missTTL = 2 * time.Minute
	}
	return &Cache{
		capacity:  capacity,
		hitTTL:    hitTTL,
		missTTL:   missTTL,
		items:     make(map[string]*list.Element),
		evictList: list.New(),
	}
}

// CacheKey constructs a cache key rounded to a time bucket (e.g. 5 seconds) to coalesce nearby flows.
func CacheKey(ispID uint32, localIP string, eventTime time.Time) string {
	// 5-second bucket
	bucket := eventTime.Unix() / 5
	return fmt.Sprintf("%d:%s:%d", ispID, localIP, bucket)
}

// Get retrieves an unexpired entry from the cache.
func (c *Cache) Get(key string) (LookupResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, found := c.items[key]
	if !found {
		return LookupResult{}, false
	}

	entry := elem.Value.(*cacheEntry)
	if time.Now().After(entry.expires) {
		c.removeElement(elem)
		return LookupResult{}, false
	}

	c.evictList.MoveToFront(elem)
	return entry.result, true
}

// Put stores a result in the cache with the appropriate TTL.
func (c *Cache) Put(key string, result LookupResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ttl := c.hitTTL
	if result.Status != StatusMatched {
		ttl = c.missTTL
	}

	expires := time.Now().Add(ttl)

	if elem, found := c.items[key]; found {
		c.evictList.MoveToFront(elem)
		entry := elem.Value.(*cacheEntry)
		entry.result = result
		entry.expires = expires
		return
	}

	if c.evictList.Len() >= c.capacity {
		c.removeOldest()
	}

	entry := &cacheEntry{key: key, result: result, expires: expires}
	elem := c.evictList.PushFront(entry)
	c.items[key] = elem
}

func (c *Cache) removeElement(elem *list.Element) {
	c.evictList.Remove(elem)
	entry := elem.Value.(*cacheEntry)
	delete(c.items, entry.key)
}

func (c *Cache) removeOldest() {
	elem := c.evictList.Back()
	if elem != nil {
		c.removeElement(elem)
	}
}
