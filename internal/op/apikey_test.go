package op

import (
	"context"
	"testing"

	"github.com/vrichv/octopus-pro/internal/db"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/utils/cache"
)

func TestAPIKeyPIIFilterEnabledPersistsAndUpdates(t *testing.T) {
	initTestDB(t)
	apiKeyCache = cache.New[int, model.APIKey](16)
	apiKeyIDMap = cache.New[string, int](16)

	ctx := context.Background()
	key := model.APIKey{
		Name:             "pii-filter",
		APIKey:           "sk-octopus-test-pii-filter",
		Enabled:          true,
		PIIFilterEnabled: true,
	}
	if err := APIKeyCreate(&key, ctx); err != nil {
		t.Fatalf("APIKeyCreate: %v", err)
	}

	byValue, err := APIKeyGetByAPIKey(key.APIKey, ctx)
	if err != nil {
		t.Fatalf("APIKeyGetByAPIKey: %v", err)
	}
	if !byValue.PIIFilterEnabled {
		t.Fatal("PIIFilterEnabled was not persisted on create")
	}

	byValue.PIIFilterEnabled = false
	if err := APIKeyUpdate(&byValue, ctx); err != nil {
		t.Fatalf("APIKeyUpdate: %v", err)
	}

	byID, err := APIKeyGet(key.ID, ctx)
	if err != nil {
		t.Fatalf("APIKeyGet: %v", err)
	}
	if byID.PIIFilterEnabled {
		t.Fatal("PIIFilterEnabled cache remained true after update")
	}

	var direct model.APIKey
	if err := db.GetDB().WithContext(ctx).First(&direct, key.ID).Error; err != nil {
		t.Fatalf("direct API key read: %v", err)
	}
	if direct.PIIFilterEnabled {
		t.Fatal("PIIFilterEnabled database value remained true after update")
	}
}
