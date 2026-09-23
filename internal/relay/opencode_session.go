package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
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

// Keep the explicit channel session caches separate from endpoint fallback sessions.
var openCodeZenFreeSessions = newOpenCodeSessionCache()

// Go uses the same header/session format while retaining an independent cache.
var openCodeGoSessions = newOpenCodeSessionCache()

func (c *openCodeSessionCache) zenFreeSessionID(key dbmodel.ChannelKey, now time.Time) (string, error) {
	fingerprint := sha256.Sum256([]byte(key.ChannelKey))
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.byKey[key.ID]
	if ok && entry.keyFingerprint == fingerprint && !now.After(entry.lastUsedAt.Add(openCodeSessionIdleTTL)) {
		entry.lastUsedAt = now
		c.byKey[key.ID] = entry
		return entry.sessionID, nil
	}

	var prefix [6]byte
	if _, err := io.ReadFull(rand.Reader, prefix[:]); err != nil {
		return "", fmt.Errorf("generate OpenCode Zen Free session: %w", err)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var suffix [14]byte
	for i := range suffix {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", fmt.Errorf("generate OpenCode Zen Free session: %w", err)
		}
		suffix[i] = alphabet[n.Int64()]
	}
	id := "ses_" + hex.EncodeToString(prefix[:]) + string(suffix[:])
	c.byKey[key.ID] = openCodeSessionEntry{sessionID: id, lastUsedAt: now, keyFingerprint: fingerprint}
	return id, nil
}

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
