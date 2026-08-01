package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lark-coding-agent-bridge-go/internal/config"
)

func TestOpenCodeReadUsage(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-opencode")
	body := `#!/bin/sh
if [ "$1" = "db" ]; then
  printf '%s\n' '[{"sessions":12,"messages":345,"input_tokens":1234,"output_tokens":56,"cache_read_tokens":789,"cache_write_tokens":10,"reasoning_tokens":11,"cost_usd":1.25}]'
  exit 0
fi
exit 2
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter := NewOpenCodeAdapter(script)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := adapter.ReadUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "OpenCode" || got.Activity == nil {
		t.Fatalf("usage=%+v", got)
	}
	if got.Activity.Sessions != 12 || got.Activity.Messages != 345 || got.Activity.CachedInputTokens != 789 || got.Activity.CostUSD != 1.25 {
		t.Fatalf("activity=%+v", got.Activity)
	}
}

func TestOpenCodeListModelsForDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/config/providers":
			if r.URL.Query().Get("directory") != "/repo" {
				http.Error(w, "bad directory", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{
  "providers":[{"id":"deepseek","name":"DeepSeek","models":{
    "chat":{"id":"chat","name":"Chat","description":"Fast"},
    "old":{"id":"old","name":"Old","status":"deprecated"}
  }}],
  "default":{"deepseek":"chat"}
}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := NewOpenCodeAdapter("opencode")
	adapter.defaultAccess = config.AccessFull
	adapter.server = &ocServer{
		base:   server.URL,
		client: server.Client(),
		access: config.AccessFull,
	}
	models, err := adapter.ListModels(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "deepseek/chat" || !models[0].Default || models[0].DisplayName != "DeepSeek · Chat" {
		t.Fatalf("models=%+v", models)
	}
}

func TestOpenCodeListSessionsAcrossProjects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/project":
			_, _ = w.Write([]byte(`[
  {"worktree":"/repo-a","sandboxes":["/repo-a-wt"]},
  {"worktree":"/repo-b","sandboxes":[]}
]`))
		case "/session":
			switch r.URL.Query().Get("directory") {
			case "/repo-a":
				_, _ = w.Write([]byte(`[
  {"id":"ses-a","title":"A","directory":"/repo-a","time":{"updated":1000}},
  {"id":"ses-shared","title":"old","directory":"/repo-a","time":{"updated":1500}}
]`))
			case "/repo-a-wt":
				_, _ = w.Write([]byte(`[
  {"id":"ses-shared","title":"new","directory":"/repo-a-wt","time":{"updated":3000}}
]`))
			case "/repo-b":
				_, _ = w.Write([]byte(`[
  {"id":"ses-b","title":"B","directory":"/repo-b","time":{"updated":2000}}
]`))
			default:
				http.Error(w, "bad directory", http.StatusBadRequest)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	srv := &ocServer{base: server.URL, client: server.Client()}
	got, err := srv.listSessionsAcrossProjects(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("sessions=%+v", got)
	}
	if got[0].ID != "ses-shared" || got[0].Preview != "new" || got[1].ID != "ses-b" || got[2].ID != "ses-a" {
		t.Fatalf("order/dedup=%+v", got)
	}
}
