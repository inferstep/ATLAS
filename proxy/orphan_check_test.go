package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// postOrphanCheck asks the V3 service which added functions nothing calls.
// Only an answer counts: a refused or unreachable service gives no orphans,
// so a service that did not answer never stops the agent at its exit.
func TestPostOrphanCheckTrustsOnlyAnAnswer(t *testing.T) {
	answered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var asked map[string]string
		_ = json.NewDecoder(r.Body).Decode(&asked)
		if r.URL.Path != "/internal/orphaned_symbols" ||
			!reflect.DeepEqual(asked, map[string]string{"path": "todo.py", "previous": "old", "source": "new"}) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"orphans": [{"name": "done_task", "line": 12}]}`))
	}))
	defer answered.Close()
	// The refusal carries orphans in its body: they must not be read.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"orphans": [{"name": "done_task", "line": 12}]}`))
	}))
	defer refused.Close()
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()

	for _, c := range []struct {
		name string
		url  string
		want []orphanedSymbol
	}{
		{"answered", answered.URL, []orphanedSymbol{{Name: "done_task", Line: 12}}},
		{"refused with 503", refused.URL, nil},
		{"unreachable", unreachable.URL, nil},
	} {
		got := postOrphanCheck(&AgentContext{V3URL: c.url}, "todo.py", "old", "new")
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
