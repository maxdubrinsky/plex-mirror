// Package mcp is the Phase 5 MCP face of plex-mirror. It exposes the mirror
// operations as MCP tools over streamable HTTP, mounted under /mcp by the
// server package. Every handler is a thin adapter over internal/service — no
// business logic lives here, so the portal (Phase 4) and this server stay two
// faces of one core.
package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxdubrinsky/plex-mirror/internal/service"
)

const (
	serverName    = "plex-mirror"
	serverVersion = "0.1.0"
)

// NewServer builds the MCP server with every plex-mirror tool registered
// against svc. Exposed (rather than only Handler) so tests can drive it
// in-process without an HTTP round-trip.
func NewServer(svc *service.Service) *server.MCPServer {
	s := server.NewMCPServer(serverName, serverVersion,
		server.WithToolCapabilities(true))
	registerTools(s, svc)
	return s
}

// Handler returns a streamable-HTTP handler for the MCP server, ready to mount
// under /mcp.
func Handler(svc *service.Service) *server.StreamableHTTPServer {
	return server.NewStreamableHTTPServer(NewServer(svc))
}

func registerTools(s *server.MCPServer, svc *service.Service) {
	s.AddTool(mcpgo.NewTool("list_sources",
		mcpgo.WithDescription("List configured media sources (e.g. plex, jellyfin) and whether each can be downloaded from."),
	), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return textResult(svc.ListSources())
	})

	s.AddTool(mcpgo.NewTool("list_libraries",
		mcpgo.WithDescription("List the libraries (movie/show/music collections) on a source."),
		mcpgo.WithString("source", mcpgo.Required(),
			mcpgo.Description("source name from list_sources, e.g. \"plex\" or \"jellyfin\"")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		src, err := req.RequireString("source")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		libs, err := svc.ListLibraries(ctx, src)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(libs)
	})

	s.AddTool(mcpgo.NewTool("list_items",
		mcpgo.WithDescription("List items in a library. Optional title search (server-side, spans the whole library); limit/offset paginate."),
		mcpgo.WithString("source", mcpgo.Required(), mcpgo.Description("source name from list_sources")),
		mcpgo.WithString("library", mcpgo.Required(), mcpgo.Description("library ID from list_libraries")),
		mcpgo.WithString("filter", mcpgo.Description("optional title search; matched server-side across the whole library")),
		mcpgo.WithNumber("limit", mcpgo.Description("max items to return (default 50)"), mcpgo.Min(1), mcpgo.Max(500)),
		mcpgo.WithNumber("offset", mcpgo.Description("items to skip (default 0)"), mcpgo.Min(0)),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		src, err := req.RequireString("source")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		lib, err := req.RequireString("library")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		items, err := svc.ListItems(ctx, src, lib, optString(req, "filter"),
			optInt(req, "limit", 50), optInt(req, "offset", 0))
		if err != nil {
			return errResult(err), nil
		}
		return textResult(items)
	})

	s.AddTool(mcpgo.NewTool("get_item",
		mcpgo.WithDescription("Fetch full metadata for a single item on a source."),
		mcpgo.WithString("source", mcpgo.Required(), mcpgo.Description("source name from list_sources")),
		mcpgo.WithString("item_id", mcpgo.Required(), mcpgo.Description("the source's item id (e.g. a Plex ratingKey)")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		src, err := req.RequireString("source")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		itemID, err := req.RequireString("item_id")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		item, err := svc.GetItem(ctx, src, itemID)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(item)
	})

	s.AddTool(mcpgo.NewTool("list_children",
		mcpgo.WithDescription("List the direct children of a container item: a show's seasons, a season's episodes. Episodes are the downloadable leaves to pass to queue_download."),
		mcpgo.WithString("source", mcpgo.Required(), mcpgo.Description("source name from list_sources")),
		mcpgo.WithString("item_id", mcpgo.Required(), mcpgo.Description("the container's item id (e.g. a Plex show or season ratingKey)")),
		mcpgo.WithNumber("limit", mcpgo.Description("max children to return (default 200)"), mcpgo.Min(1), mcpgo.Max(2000)),
		mcpgo.WithNumber("offset", mcpgo.Description("children to skip (default 0)"), mcpgo.Min(0)),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		src, err := req.RequireString("source")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		itemID, err := req.RequireString("item_id")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		items, err := svc.ListChildren(ctx, src, itemID, optInt(req, "limit", 200), optInt(req, "offset", 0))
		if err != nil {
			return errResult(err), nil
		}
		return textResult(items)
	})

	s.AddTool(mcpgo.NewTool("queue_download",
		mcpgo.WithDescription("Queue an item for download to the local mirror. Plex only. The download runs in the background; poll download_status for progress."),
		mcpgo.WithString("source", mcpgo.Required(), mcpgo.Description("source name; only \"plex\" supports downloads today")),
		mcpgo.WithString("item_id", mcpgo.Required(), mcpgo.Description("the source's item id (Plex ratingKey)")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		src, err := req.RequireString("source")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		itemID, err := req.RequireString("item_id")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		item, err := svc.QueueDownload(ctx, src, itemID)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(item)
	})

	s.AddTool(mcpgo.NewTool("queue_container",
		mcpgo.WithDescription("Queue every downloadable episode beneath a container — a season (its episodes) or a show (all seasons → episodes). Plex only. Idempotent: episodes already mirrored or in flight are skipped. Returns counts (queued/skipped/failed) + total bytes."),
		mcpgo.WithString("source", mcpgo.Required(), mcpgo.Description("source name; only \"plex\" supports downloads today")),
		mcpgo.WithString("item_id", mcpgo.Required(), mcpgo.Description("the container's item id (a Plex show or season ratingKey)")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		src, err := req.RequireString("source")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		itemID, err := req.RequireString("item_id")
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		res, err := svc.QueueContainer(ctx, src, itemID)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(res)
	})

	s.AddTool(mcpgo.NewTool("download_status",
		mcpgo.WithDescription("Report download progress. With item_id, returns that one item; without, returns all in-flight downloads (queued/downloading/error)."),
		mcpgo.WithNumber("item_id", mcpgo.Description("local item id (from queue_download); omit for all in-flight"), mcpgo.Min(1)),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		var idPtr *int64
		if id, ok := optInt64(req, "item_id"); ok {
			idPtr = &id
		}
		items, err := svc.DownloadStatus(ctx, idPtr)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(items)
	})

	s.AddTool(mcpgo.NewTool("list_mirrored",
		mcpgo.WithDescription("List items already mirrored locally (status ready), newest first. Optional case-insensitive title filter."),
		mcpgo.WithString("filter", mcpgo.Description("optional case-insensitive substring matched against titles")),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		items, err := svc.ListMirrored(ctx, optString(req, "filter"))
		if err != nil {
			return errResult(err), nil
		}
		return textResult(items)
	})

	s.AddTool(mcpgo.NewTool("get_config",
		mcpgo.WithDescription("Report the current effective configuration (sources, storage caps, download tunables). Read-only: credential values are never returned — only booleans saying whether a Plex/Jellyfin token is set. Change config from the web portal's Settings page."),
	), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		cv, err := svc.ConfigView(ctx)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(cv)
	})

	s.AddTool(mcpgo.NewTool("storage_stats",
		mcpgo.WithDescription("Report local storage: bytes used, free space, configured caps, and count of ready items."),
	), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		stats, err := svc.StorageStats(ctx)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(stats)
	})

	s.AddTool(mcpgo.NewTool("evict",
		mcpgo.WithDescription("Manually evict a mirrored item by local id: delete its file and free the space."),
		mcpgo.WithNumber("item_id", mcpgo.Required(), mcpgo.Description("local item id to evict (from list_mirrored)"), mcpgo.Min(1)),
	), func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		id, ok := optInt64(req, "item_id")
		if !ok {
			return mcpgo.NewToolResultError("item_id is required (integer)"), nil
		}
		ev, err := svc.Evict(ctx, id)
		if err != nil {
			return errResult(err), nil
		}
		return textResult(ev)
	})
}

// textResult marshals v to indented JSON and wraps it as tool text output.
func textResult(v any) (*mcpgo.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcpgo.NewToolResultErrorFromErr("marshal result", err), nil
	}
	return mcpgo.NewToolResultText(string(b)), nil
}

// errResult surfaces a service/source error to the model as a tool error. The
// service layer already wraps its sentinels with descriptive context, so the
// message is meaningful as-is.
func errResult(err error) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultError(err.Error())
}

func optString(req mcpgo.CallToolRequest, key string) string {
	if v, ok := req.GetArguments()[key].(string); ok {
		return v
	}
	return ""
}

func optInt(req mcpgo.CallToolRequest, key string, def int) int {
	if v, ok := optInt64(req, key); ok {
		return int(v)
	}
	return def
}

// optInt64 pulls an integer argument. JSON numbers decode to float64 over the
// wire, so that's the common case; the rest are defensive for in-process tests.
func optInt64(req mcpgo.CallToolRequest, key string) (int64, bool) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}
