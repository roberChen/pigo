package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchRemoteModelsOpenAICompatible verifies the OpenAI-compatible path:
// GET {base}/models with a Bearer header, ids parsed, deduplicated, sorted.
func TestFetchRemoteModelsOpenAICompatible(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3"},{"id":"glm-4.7"},{"id":"glm-5.3"}]}`))
	}))
	defer srv.Close()

	ids, err := FetchRemoteModels(context.Background(), srv.URL+"/v1", "openai", "test-key")
	if err != nil {
		t.Fatalf("FetchRemoteModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if len(ids) != 2 || ids[0] != "glm-4.7" || ids[1] != "glm-5.3" {
		t.Errorf("ids = %v, want deduplicated and sorted [glm-4.7 glm-5.3]", ids)
	}
}

// TestFetchRemoteModelsAnthropic verifies the Anthropic path: the catalog sits at
// {base}/models with x-api-key + anthropic-version headers, where {base} is the
// same value the driver posts to.
//
// The regression this pins: base already carries the version segment (the built-in
// anthropic provider's base is "https://api.anthropic.com/v1", and the Messages
// driver posts to {base}/messages), so appending "/v1/models" produced
// GET /v1/v1/models — a 404 on every Anthropic endpoint, built-in and custom.
func TestFetchRemoteModelsAnthropic(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-fable-5","display_name":"Claude Fable 5"}]}`))
	}))
	defer srv.Close()

	// A base with the version segment, exactly as the driver receives it.
	ids, err := FetchRemoteModels(context.Background(), srv.URL+"/v1", "anthropic", "test-key")
	if err != nil {
		t.Fatalf("FetchRemoteModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models (a version segment must not be appended again)", gotPath)
	}
	if gotKey != "test-key" || gotVersion != anthropicAPIVersion {
		t.Errorf("headers = (%q, %q), want x-api-key + version", gotKey, gotVersion)
	}
	if len(ids) != 1 || ids[0] != "claude-fable-5" {
		t.Errorf("ids = %v, want [claude-fable-5]", ids)
	}
}

// TestFetchRemoteModelsFailures verifies graceful failure reporting: a missing
// base URL, an unknown protocol, server errors, malformed bodies, and that an
// empty key sends no auth header at all.
func TestFetchRemoteModelsFailures(t *testing.T) {
	if _, err := FetchRemoteModels(context.Background(), "  ", "openai", "k"); err == nil {
		t.Error("empty base URL succeeded, want error")
	}
	if _, err := FetchRemoteModels(context.Background(), "http://x", "carrier-pigeon", "k"); err == nil {
		t.Error("unknown protocol succeeded, want error")
	}

	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") != ""
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	// An empty key sends no auth header at all (local gateways like Ollama).
	if _, err := FetchRemoteModels(context.Background(), srv.URL, "openai", ""); err == nil {
		t.Error("401 response succeeded, want error")
	}
	if sawAuth {
		t.Error("empty key sent an Authorization header, want none")
	}
	if _, err := FetchRemoteModels(context.Background(), srv.URL, "openai", "k"); err == nil {
		t.Error("401 response succeeded, want error")
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv2.Close()
	if _, err := FetchRemoteModels(context.Background(), srv2.URL, "openai", "k"); err == nil || !strings.Contains(err.Error(), "response shape") {
		t.Errorf("malformed body error = %v, want response-shape failure", err)
	}
}

// TestFetchRemoteModelsEmptyCatalogIsFailure verifies that a 200 with no standard
// "data" entries is an error, not an empty success.
//
// A bespoke catalog API answers 200 in its own shape (here {"result":...}); that
// decodes cleanly into a zero-value Data, so returning (nil, nil) made the command
// report "fetched 0 models" and overwrite the session catalog with nothing. The
// recipe for such a gateway is out of band (a pigorc registers the ids), so the
// message has to say that much rather than look like a network hiccup.
func TestFetchRemoteModelsEmptyCatalogIsFailure(t *testing.T) {
	for name, body := range map[string]string{
		"bespoke shape": `{"result":{"models":[{"key":"corp-a"}]}}`,
		"empty data":    `{"data":[]}`,
		"blank ids":     `{"data":[{"id":""}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			ids, err := FetchRemoteModels(context.Background(), srv.URL+"/v1", "openai", "k")
			if err == nil {
				t.Fatalf("ids = %v, want a failure: an empty catalog must not read as success", ids)
			}
			if !strings.Contains(err.Error(), "no models") || !strings.Contains(err.Error(), "pigorc") {
				t.Errorf("error = %v, want it to name the empty catalog and the pigorc fallback", err)
			}
		})
	}
}
