package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchIPCountrySupportsIPv6(t *testing.T) {
	const ipv6 = "2603:c022:8000:dc00::1:3000"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+ipv6 {
			t.Fatalf("country lookup path = %q, want %q", r.URL.Path, "/"+ipv6)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"country": "South Korea",
		})
	}))
	defer server.Close()

	previousEndpoint := exitIPCountryEndpoint
	exitIPCountryEndpoint = server.URL + "/"
	t.Cleanup(func() { exitIPCountryEndpoint = previousEndpoint })

	country, err := fetchIPCountry(context.Background(), server.Client(), ipv6)
	if err != nil {
		t.Fatalf("fetchIPCountry: %v", err)
	}
	if country != "South Korea" {
		t.Fatalf("country = %q, want %q", country, "South Korea")
	}
}
