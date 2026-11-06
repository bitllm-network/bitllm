package node

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDiscoverModelsKeepsExactIDs: the upstream id is the network name. A
// Hugging Face repo id has a slash and keeps its case; a private model is
// just another id. Nothing is lowercased or rewritten.
func TestDiscoverModelsKeepsExactIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"Qwen/Qwen3.8-Flash-Next"},{"id":"my-lab/new-model"}]}`))
	}))
	defer srv.Close()

	n := &Node{cfg: Config{Upstream: srv.URL, ModelsTimeout: 5 * time.Second}, http: srv.Client()}
	got, err := n.discoverModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"Qwen/Qwen3.8-Flash-Next", "my-lab/new-model"} {
		if got[id] != id {
			t.Fatalf("discovered[%q] = %q, want itself", id, got[id])
		}
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d models, want 2", len(got))
	}
}

// TestDiscoverModelsCapsTheRead: an answer past the cap is an error, not memory
// spent on it, even when it is valid JSON.
func TestDiscoverModelsCapsTheRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":[{"id":"m"}],"pad":"%s"}`, strings.Repeat("x", maxModelList))
	}))
	defer srv.Close()

	n := &Node{cfg: Config{Upstream: srv.URL, ModelsTimeout: 5 * time.Second}, http: srv.Client()}
	if got, err := n.discoverModels(context.Background()); err == nil {
		t.Fatalf("read an answer past the cap: %v", got)
	}
}

// TestModelsCanBeReplaced: a running node can pick up a new model without a
// restart, and the old name stops being served.
func TestModelsCanBeReplaced(t *testing.T) {
	n := &Node{models: map[string]string{"old": "up-old"}}
	n.setModels(map[string]string{"Qwen/Qwen3.8-Flash-Next": "up-qwen"})

	if _, ok := n.upstreamModel("old"); ok {
		t.Fatal("old model is still served")
	}
	if up, ok := n.upstreamModel("Qwen/Qwen3.8-Flash-Next"); !ok || up != "up-qwen" {
		t.Fatalf("new model = %q, %v; want up-qwen, true", up, ok)
	}
	if names := n.modelNames(); len(names) != 1 || names[0] != "Qwen/Qwen3.8-Flash-Next" {
		t.Fatalf("modelNames = %v", names)
	}
}
