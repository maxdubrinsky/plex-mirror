package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/service"
)

// rpcResponse is the slice of the JSON-RPC envelope the tests inspect.
type rpcResponse struct {
	Result struct {
		// tools/list
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		// tools/call
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func newTestServer(t *testing.T) *mcpserver.MCPServer {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	// No source creds → empty sources, nil engine. Enough to exercise the MCP
	// adapter layer (registration, arg parsing, JSON shaping, error surfacing).
	svc, err := service.New(&config.Config{MediaRoot: t.TempDir()}, store)
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	return NewServer(svc)
}

func rpc(t *testing.T, srv *mcpserver.MCPServer, method string, params any) rpcResponse {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	msg := srv.HandleMessage(context.Background(), raw)
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var out rpcResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal response %s: %v", b, err)
	}
	return out
}

func TestToolsListRegistersAllTools(t *testing.T) {
	srv := newTestServer(t)
	out := rpc(t, srv, "tools/list", nil)
	if out.Error != nil {
		t.Fatalf("tools/list error: %s", out.Error.Message)
	}

	want := map[string]bool{
		"list_sources": true, "list_libraries": true, "list_items": true,
		"get_item": true, "list_children": true, "queue_download": true,
		"queue_container": true, "download_status": true, "list_mirrored": true,
		"storage_stats": true, "evict": true, "get_config": true,
		"source_health": true, "reconnect_source": true,
	}
	got := map[string]bool{}
	for _, tool := range out.Result.Tools {
		got[tool.Name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tools %v, want %d %v", len(got), got, len(want), want)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing tool %q", name)
		}
	}
}

func TestCallListSourcesEmpty(t *testing.T) {
	srv := newTestServer(t)
	out := rpc(t, srv, "tools/call", map[string]any{
		"name":      "list_sources",
		"arguments": map[string]any{},
	})
	if out.Error != nil {
		t.Fatalf("rpc error: %s", out.Error.Message)
	}
	if out.Result.IsError {
		t.Fatalf("tool reported error: %+v", out.Result.Content)
	}
	if len(out.Result.Content) == 0 {
		t.Fatal("no content returned")
	}
	// No sources configured → empty JSON array.
	var sources []service.SourceInfo
	if err := json.Unmarshal([]byte(out.Result.Content[0].Text), &sources); err != nil {
		t.Fatalf("decode content %q: %v", out.Result.Content[0].Text, err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %+v, want empty", sources)
	}
}

func TestCallStorageStats(t *testing.T) {
	srv := newTestServer(t)
	out := rpc(t, srv, "tools/call", map[string]any{
		"name":      "storage_stats",
		"arguments": map[string]any{},
	})
	if out.Error != nil || out.Result.IsError {
		t.Fatalf("storage_stats failed: err=%v content=%+v", out.Error, out.Result.Content)
	}
	var stats service.StorageStats
	if err := json.Unmarshal([]byte(out.Result.Content[0].Text), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.ItemsReady != 0 || stats.UsedBytes != 0 {
		t.Fatalf("stats = %+v, want zero usage", stats)
	}
}

func TestCallUnknownSourceIsToolError(t *testing.T) {
	srv := newTestServer(t)
	out := rpc(t, srv, "tools/call", map[string]any{
		"name":      "list_libraries",
		"arguments": map[string]any{"source": "plex"}, // not configured in this test
	})
	if out.Error != nil {
		t.Fatalf("unexpected rpc-level error: %s", out.Error.Message)
	}
	if !out.Result.IsError {
		t.Fatalf("expected tool error for unconfigured source, got %+v", out.Result.Content)
	}
}

func TestCallMissingRequiredArgIsToolError(t *testing.T) {
	srv := newTestServer(t)
	out := rpc(t, srv, "tools/call", map[string]any{
		"name":      "list_libraries",
		"arguments": map[string]any{}, // missing required "source"
	})
	if out.Error != nil {
		t.Fatalf("unexpected rpc-level error: %s", out.Error.Message)
	}
	if !out.Result.IsError {
		t.Fatalf("expected tool error for missing source arg, got %+v", out.Result.Content)
	}
}
