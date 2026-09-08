package relay

import (
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
)

const openCodeSessionIdleTTL = time.Hour

type openCodeSessionEntry struct {
	sessionID      string
	lastUsedAt     time.Time
	keyFingerprint [sha256.Size]byte
}

type openCodeSessionCache struct {
	mu    sync.Mutex
	byKey map[int]openCodeSessionEntry
}

func newOpenCodeSessionCache() *openCodeSessionCache {
	return &openCodeSessionCache{byKey: make(map[int]openCodeSessionEntry)}
}

var openCodeSessions = newOpenCodeSessionCache()

// sessionID returns the current OpenCode routing session for a specific
// outbound key. Replacing the configured key or leaving the session unused
// for more than one hour rotates the UUID on that key's next use.
func (c *openCodeSessionCache) sessionID(key dbmodel.ChannelKey, now time.Time) string {
	fingerprint := sha256.Sum256([]byte(key.ChannelKey))

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.byKey[key.ID]
	if ok && entry.keyFingerprint == fingerprint && !now.After(entry.lastUsedAt.Add(openCodeSessionIdleTTL)) {
		entry.lastUsedAt = now
		c.byKey[key.ID] = entry
		return entry.sessionID
	}

	sessionID := uuid.NewString()
	c.byKey[key.ID] = openCodeSessionEntry{
		sessionID:      sessionID,
		lastUsedAt:     now,
		keyFingerprint: fingerprint,
	}
	return sessionID
}

func isOpenCodeEndpoint(rawURL string) bool {
	return strings.Contains(strings.ToLower(rawURL), "opencode.ai")
}
