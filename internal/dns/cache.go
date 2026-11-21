package dns

import (
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Cache bounds.
const (
	maxEntries = 4096
	maxTTL     = time.Hour // fresh for at most this long
	staleFor   = time.Hour // then served only while upstream fails
	noTTL      = 30 * time.Second
)

// cache holds forwarded answers, so a box keeps resolving the names it
// uses through an upstream that drops packets for a few seconds: a
// fresh answer is served without asking, and a stale one when asking
// fails.
type cache struct {
	mu      sync.Mutex
	entries map[string]entry
}

type entry struct {
	msg          []byte
	fresh, stale time.Time
}

// get returns the answer for key with its ID set to id, and whether it
// is still fresh. A missing or too old answer is nil.
func (c *cache) get(key string, id uint16) (msg []byte, fresh bool) {
	c.mu.Lock()
	e, ok := c.entries[key]
	c.mu.Unlock()
	now := time.Now()
	if !ok || now.After(e.stale) {
		return nil, false
	}
	msg = append([]byte(nil), e.msg...)
	msg[0], msg[1] = byte(id>>8), byte(id)
	return msg, now.Before(e.fresh)
}

// put stores msg under key for as long as its records live. Truncated
// answers and failures other than "no such name" aren't stored.
func (c *cache) put(key string, msg []byte) {
	ttl, ok := cacheTTL(msg)
	if !ok {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= maxEntries {
		c.entries = map[string]entry{}
	}
	c.entries[key] = entry{append([]byte(nil), msg...), now.Add(ttl), now.Add(ttl + staleFor)}
}

// cacheTTL is the lowest TTL among msg's answer and authority records
// (for "no such name" or "no records", the SOA's), at most maxTTL, and
// whether msg may be cached at all.
func cacheTTL(msg []byte) (time.Duration, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || h.Truncated || (h.RCode != dnsmessage.RCodeSuccess && h.RCode != dnsmessage.RCodeNameError) {
		return 0, false
	}
	if err := p.SkipAllQuestions(); err != nil {
		return 0, false
	}
	ttl, seen := maxTTL, false
	note := func(r dnsmessage.ResourceHeader) {
		seen = true
		ttl = min(ttl, time.Duration(r.TTL)*time.Second)
	}
	for {
		r, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		} else if err != nil {
			return 0, false
		}
		note(r)
		if err := p.SkipAnswer(); err != nil {
			return 0, false
		}
	}
	for {
		r, err := p.AuthorityHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		} else if err != nil {
			return 0, false
		}
		note(r)
		if err := p.SkipAuthority(); err != nil {
			return 0, false
		}
	}
	if !seen {
		ttl = noTTL
	}
	return ttl, true
}
