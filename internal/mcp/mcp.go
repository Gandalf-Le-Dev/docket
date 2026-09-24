// Package mcp serves Docket's MCP endpoint over Streamable HTTP.
//
// The server is dual-era, per the 2026-07-28 spec's compatibility model:
//   - Modern clients (revision 2026-07-28) send per-request metadata — the
//     MCP-Protocol-Version and Mcp-Method/Mcp-Name headers plus _meta in the
//     body — and may call server/discover. No handshake, no sessions.
//   - Legacy clients (2025-03-26 through 2025-11-25) open with an initialize
//     handshake. Docket answers it but keeps zero per-client state and never
//     mints an Mcp-Session-Id, which those revisions permit.
//
// Every response is a single plain JSON object; Docket never streams and
// never pushes, so no SSE and no GET endpoint.
package mcp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Gandalf-Le-Dev/docket/internal/link"
	"github.com/Gandalf-Le-Dev/docket/internal/store"
)

const ModernVersion = "2026-07-28"

// cacheTTLMs rides on the results the 2026-07-28 revision declares cacheable
// (server/discover and tools/list here), which must carry caching hints. Five
// minutes, and always cacheScope "private": both results vary by the token's
// role — a shared cache must never serve one token's tool surface to another.
// tools/call and ping are not cacheable and carry no hints.
const cacheTTLMs = 300_000

var legacyVersions = map[string]bool{
	"2025-03-26": true,
	"2025-06-18": true,
	"2025-11-25": true,
}

// SupportedVersions is what server/discover and version errors advertise.
var SupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// JSON-RPC error codes. -32020 and -32022 are allocated by the MCP spec.
const (
	codeParseError         = -32700
	codeInvalidRequest     = -32600
	codeMethodNotFound     = -32601
	codeInvalidParams      = -32602
	codeHeaderMismatch     = -32020
	codeUnsupportedVersion = -32022
)

// AddRateLimit bounds how much noise one compromised publish token can make.
const (
	AddRateLimit  = 120
	AddRateWindow = time.Hour
)

type Handler struct {
	Store   *store.Store
	Version string
	// PublicURL pins the origin item links are built on; empty means the
	// origin each request arrived on (see link.Base).
	PublicURL string

	mu   sync.Mutex
	adds map[string][]time.Time
}

func NewHandler(s *store.Store, version, publicURL string) *Handler {
	return &Handler{Store: s, Version: version, PublicURL: publicURL, adds: map[string][]time.Time{}}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (r *rpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	e := map[string]any{"code": code, "message": message}
	if data != nil {
		e["data"] = data
	}
	writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": e})
}

// decodeHeaderValue undoes the spec's Base64 sentinel encoding
// (`=?base64?...?=`) used when a header value is not header-safe ASCII.
func decodeHeaderValue(v string) string {
	if strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=") {
		if raw, err := base64.StdEncoding.DecodeString(v[len("=?base64?") : len(v)-2]); err == nil {
			return string(raw)
		}
	}
	return v
}

func metaProtocolVersion(params json.RawMessage) string {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	var v string
	json.Unmarshal(p.Meta["io.modelcontextprotocol/protocolVersion"], &v)
	return v
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// The 2026-07-28 revision removed the GET stream and DELETE session
		// teardown; older clients sending them get 405, which every revision
		// permits for a server that offers neither.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tokenName, role, ok := h.authenticate(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, nil, codeParseError, "parse error: "+err.Error(), nil)
		return
	}

	// Notifications (e.g. a legacy client's notifications/initialized) are
	// acknowledged and dropped — there is nothing stateful to do with them.
	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	headerVer := r.Header.Get("MCP-Protocol-Version")
	switch {
	case headerVer == ModernVersion:
		if !h.validateModernHeaders(w, r, &req) {
			return
		}
	case headerVer == "" || legacyVersions[headerVer]:
		// Legacy era. An absent header is treated as 2025-03-26, which the
		// spec allows for servers that support pre-2025-06-18 clients.
	default:
		writeError(w, http.StatusBadRequest, req.ID, codeUnsupportedVersion, "Unsupported protocol version",
			map[string]any{"supported": SupportedVersions, "requested": headerVer})
		return
	}

	switch req.Method {
	case "server/discover":
		writeResult(w, req.ID, map[string]any{
			"resultType":        "complete",
			"ttlMs":             cacheTTLMs,
			"cacheScope":        "private",
			"supportedVersions": SupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "docket", "version": h.Version},
			},
			"instructions": h.instructions(role),
		})

	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		negotiated := p.ProtocolVersion
		if !legacyVersions[negotiated] {
			// Requested something we don't speak handshake-style; answer with
			// our newest legacy revision, per the legacy negotiation rules.
			negotiated = "2025-06-18"
		}
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "docket", "title": "Docket", "version": h.Version},
			"instructions":    h.instructions(role),
		})

	case "ping":
		writeResult(w, req.ID, map[string]any{"resultType": "complete"})

	case "tools/list":
		writeResult(w, req.ID, map[string]any{
			"resultType": "complete",
			"ttlMs":      cacheTTLMs,
			"cacheScope": "private",
			"tools":      toolDefs(role),
		})

	case "tools/call":
		h.handleToolCall(w, r, &req, tokenName, role)

	default:
		// Modern servers signal an unknown method with HTTP 404 so a legacy
		// client's probe can tell "modern server" from "no MCP here"; in the
		// legacy era the JSON-RPC error rides a 200 as it always did.
		status := http.StatusOK
		if headerVer == ModernVersion {
			status = http.StatusNotFound
		}
		writeError(w, status, req.ID, codeMethodNotFound, "Method not found: "+req.Method, nil)
	}
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (name, role string, ok bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing bearer token"})
		return "", "", false
	}
	name, role, err := h.Store.Auth(strings.TrimSpace(strings.TrimPrefix(auth, prefix)))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "auth failure"})
		}
		return "", "", false
	}
	return name, role, true
}

// validateModernHeaders enforces the 2026-07-28 request-metadata rules: the
// version header must match _meta, Mcp-Method must match the body method, and
// tools/call must carry a matching Mcp-Name.
func (h *Handler) validateModernHeaders(w http.ResponseWriter, r *http.Request, req *rpcRequest) bool {
	fail := func(msg string) bool {
		writeError(w, http.StatusBadRequest, req.ID, codeHeaderMismatch, "Header mismatch: "+msg, nil)
		return false
	}
	if v := metaProtocolVersion(req.Params); v != ModernVersion {
		return fail(fmt.Sprintf("MCP-Protocol-Version header value %q does not match _meta protocol version %q", ModernVersion, v))
	}
	m := r.Header.Get("Mcp-Method")
	if m == "" {
		return fail("required Mcp-Method header is missing")
	}
	if decodeHeaderValue(m) != req.Method {
		return fail(fmt.Sprintf("Mcp-Method header value %q does not match body method %q", m, req.Method))
	}
	if req.Method == "tools/call" {
		var p struct {
			Name string `json:"name"`
		}
		json.Unmarshal(req.Params, &p)
		n := r.Header.Get("Mcp-Name")
		if n == "" {
			return fail("required Mcp-Name header is missing")
		}
		if decodeHeaderValue(n) != p.Name {
			return fail(fmt.Sprintf("Mcp-Name header value %q does not match body value %q", n, p.Name))
		}
	}
	return true
}

func (h *Handler) instructions(role string) string {
	if role == "publish" {
		return "Docket is the owner's cross-project backlog. When you notice work that is real " +
			"but out of scope for the repo or conversation you are in, file it with todo_add " +
			"instead of dropping it. Always set source to where you noticed it (repo, " +
			"conversation, URL) — who noticed this is most of the context. Keep the title short; " +
			"detail goes in body. The result carries the item's url, the link to give a person."
	}
	return "Docket is the owner's cross-project backlog. Agents file items with todo_add; " +
		"review, edit, and close them from here with todo_list, todo_get, todo_update, " +
		"todo_close, and todo_scopes. Every item carries url, the link to hand to a person " +
		"or paste into a commit message. Closing with outcome=dropped records a deliberate " +
		"won't-do verdict, which is worth preferring over deleting."
}

// --- tools ---

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var addTool = toolDef{
	Name: "todo_add",
	Description: "File an item on the owner's cross-project backlog. Use this when you notice work " +
		"that is real but out of scope for the current repo or conversation, so it is not dropped. " +
		"Always set source to where you noticed it (repo name, conversation, URL). If an open item " +
		"already has the same title and scope, its id is returned with duplicate=true instead of " +
		"filing a second copy. The result carries the item's url, the link to give a person.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"title":  {"type": "string", "description": "Short imperative summary of the work (max 500 bytes)"},
			"body":   {"type": "string", "description": "Optional detail: context, links, why it matters (max 64KB)"},
			"scope":  {"type": "string", "description": "Freeform grouping, e.g. a project name or 'personal'; normalized to lowercase"},
			"source": {"type": "string", "description": "Where this was noticed: repo, conversation, URL. Always set it."}
		},
		"required": ["title"]
	}`),
}

var reviewTools = []toolDef{
	{
		Name:        "todo_list",
		Description: "Read the backlog. state defaults to open; use all to include closed items. q is a substring match over title and body.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"scope": {"type": "string", "description": "Only items in this scope"},
				"state": {"type": "string", "enum": ["open", "done", "dropped", "all"], "description": "Defaults to open"},
				"q":     {"type": "string", "description": "Substring match over title and body"}
			}
		}`),
	},
	{
		Name: "todo_get",
		Description: "Fetch one backlog item by id. Images the body shows as ![alt](/image/N) come back as image content after the text: " +
			"up to 5, each at most 5 MB and 10 MB in all (sizes of the base64). Images left out are listed by /image/N path in a closing text note.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"id": {"type": "integer"}},
			"required": ["id"]
		}`),
	},
	{
		Name:        "todo_update",
		Description: "Edit an item's title, body, scope, or state. Setting state back to open reopens a closed item.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id":    {"type": "integer"},
				"title": {"type": "string"},
				"body":  {"type": "string"},
				"scope": {"type": "string"},
				"state": {"type": "string", "enum": ["open", "done", "dropped"]}
			},
			"required": ["id"]
		}`),
	},
	{
		Name:        "todo_close",
		Description: "Close an item with a verdict: done (default) or dropped. Dropped means deliberately not doing this — prefer it over deleting. reason is appended to the body.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id":      {"type": "integer"},
				"outcome": {"type": "string", "enum": ["done", "dropped"], "description": "Defaults to done"},
				"reason":  {"type": "string", "description": "Why it was closed; appended to the body"}
			},
			"required": ["id"]
		}`),
	},
	{
		Name:        "todo_scopes",
		Description: "List the scopes in use, each with its open item count.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	},
}

// toolDefs is filtered by role: a publish token sees only todo_add and cannot
// discover that the rest exist. Publish is cheap, read is privileged.
func toolDefs(role string) []toolDef {
	if role == "publish" {
		return []toolDef{addTool}
	}
	return append([]toolDef{addTool}, reviewTools...)
}

type callParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Every result carries resultType. The 2026-07-28 revision requires it on
// each response ("complete", vs "input_required" for multi-round-trip tools),
// and modern clients reject results without it — absent-means-complete
// leniency applies only to servers speaking earlier revisions, which docket's
// modern surface is not. Legacy clients ignore the extra field, so it is
// unconditional rather than era-gated.

// toolError is the MCP convention for a failure the model should see and
// adapt to, as opposed to a protocol error.
func toolError(w http.ResponseWriter, id json.RawMessage, msg string) {
	writeResult(w, id, map[string]any{
		"resultType": "complete",
		"content":    []map[string]any{{"type": "text", "text": msg}},
		"isError":    true,
	})
}

// toolResult answers with v as text and as structured content; extra content
// blocks, such as images, follow the text.
func toolResult(w http.ResponseWriter, id json.RawMessage, v any, extra ...map[string]any) {
	text, _ := json.Marshal(v)
	writeResult(w, id, map[string]any{
		"resultType":        "complete",
		"content":           append([]map[string]any{{"type": "text", "text": string(text)}}, extra...),
		"structuredContent": v,
	})
}

// These bound how much a single todo_get puts in an agent's context. Sizes
// are of the base64 text, which is what the client receives: 5 MB of it is
// about 3.75 MB of image.
const (
	maxImageBlocks       = 5
	maxImageBlockBytes   = 5 << 20
	maxImagePayloadBytes = 10 << 20
)

// imageBlocks are the images a body shows, as MCP image content, so an agent
// sees the screenshot rather than a path it cannot fetch. At most
// maxImageBlocks images are read, oversized ones included, and none once the
// payload is spent. Whatever is left out is named in one closing text block,
// so the agent knows it is there.
func (h *Handler) imageBlocks(body string) []map[string]any {
	var blocks []map[string]any
	var left []string
	read, payload, spent := 0, 0, false
	for _, id := range store.ImageRefs(body) {
		path := fmt.Sprintf("/image/%d", id)
		if read == maxImageBlocks || spent {
			left = append(left, path)
			continue
		}
		mime, data, err := h.Store.GetImage(id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				log.Printf("mcp: read image %d: %v", id, err)
			}
			continue
		}
		read++
		size := base64.StdEncoding.EncodedLen(len(data))
		if size > maxImageBlockBytes || payload+size > maxImagePayloadBytes {
			spent = size <= maxImageBlockBytes
			left = append(left, path)
			continue
		}
		payload += size
		blocks = append(blocks, map[string]any{
			"type":     "image",
			"data":     base64.StdEncoding.EncodeToString(data),
			"mimeType": mime,
		})
	}
	if len(left) > 0 {
		noun := "images"
		if len(left) == 1 {
			noun = "image"
		}
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": fmt.Sprintf("%d %s left out to keep this result small: %s", len(left), noun, strings.Join(left, ", ")),
		})
	}
	return blocks
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

func intArg(args map[string]any, key string) (int64, bool) {
	switch v := args[key].(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func (h *Handler) allowAdd(tokenName string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := time.Now().Add(-AddRateWindow)
	kept := h.adds[tokenName][:0]
	for _, t := range h.adds[tokenName] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= AddRateLimit {
		h.adds[tokenName] = kept
		return false
	}
	h.adds[tokenName] = append(kept, time.Now())
	return true
}

func (h *Handler) handleToolCall(w http.ResponseWriter, r *http.Request, req *rpcRequest, tokenName, role string) {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeError(w, http.StatusOK, req.ID, codeInvalidParams, "invalid params: "+err.Error(), nil)
		return
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}

	allowed := map[string]bool{"todo_add": true}
	if role != "publish" {
		for _, t := range reviewTools {
			allowed[t.Name] = true
		}
	}
	if !allowed[p.Name] {
		// Same answer for "doesn't exist" and "not yours to call": a publish
		// token must not learn the backlog's surface by probing.
		writeError(w, http.StatusOK, req.ID, codeInvalidParams, "unknown tool: "+p.Name, nil)
		return
	}

	fail := func(err error) {
		var ve store.ValidationError
		switch {
		case errors.As(err, &ve):
			toolError(w, req.ID, ve.Error())
		case errors.Is(err, store.ErrNotFound):
			toolError(w, req.ID, "no todo with that id")
		default:
			writeError(w, http.StatusInternalServerError, req.ID, codeInvalidRequest, "storage failure", nil)
		}
	}

	base := link.Base(r, h.PublicURL)
	switch p.Name {
	case "todo_add":
		if !h.allowAdd(tokenName) {
			toolError(w, req.ID, fmt.Sprintf("rate limit exceeded: at most %d adds per hour per token", AddRateLimit))
			return
		}
		id, dup, err := h.Store.AddTodo(
			stringArg(p.Arguments, "title"),
			stringArg(p.Arguments, "body"),
			stringArg(p.Arguments, "scope"),
			stringArg(p.Arguments, "source"),
			tokenName,
		)
		if err != nil {
			fail(err)
			return
		}
		toolResult(w, req.ID, map[string]any{"id": id, "duplicate": dup, "url": link.Item(base, id)})

	case "todo_list":
		todos, err := h.Store.ListTodos(
			stringArg(p.Arguments, "state"),
			stringArg(p.Arguments, "scope"),
			stringArg(p.Arguments, "q"),
		)
		if err != nil {
			fail(err)
			return
		}
		out := make([]map[string]any, 0, len(todos))
		for _, t := range todos {
			out = append(out, todoJSON(t, base))
		}
		toolResult(w, req.ID, map[string]any{"todos": out})

	case "todo_get":
		id, ok := intArg(p.Arguments, "id")
		if !ok {
			toolError(w, req.ID, "id is required and must be an integer")
			return
		}
		t, err := h.Store.GetTodo(id)
		if err != nil {
			fail(err)
			return
		}
		toolResult(w, req.ID, map[string]any{"todo": todoJSON(t, base)}, h.imageBlocks(t.Body)...)

	case "todo_update":
		id, ok := intArg(p.Arguments, "id")
		if !ok {
			toolError(w, req.ID, "id is required and must be an integer")
			return
		}
		var u store.TodoUpdate
		for key, dst := range map[string]**string{"title": &u.Title, "body": &u.Body, "scope": &u.Scope, "state": &u.State} {
			if raw, present := p.Arguments[key]; present {
				s, isStr := raw.(string)
				if !isStr {
					toolError(w, req.ID, key+" must be a string")
					return
				}
				*dst = &s
			}
		}
		t, err := h.Store.UpdateTodo(id, u)
		if err != nil {
			fail(err)
			return
		}
		toolResult(w, req.ID, map[string]any{"todo": todoJSON(t, base)})

	case "todo_close":
		id, ok := intArg(p.Arguments, "id")
		if !ok {
			toolError(w, req.ID, "id is required and must be an integer")
			return
		}
		t, err := h.Store.CloseTodo(id, stringArg(p.Arguments, "outcome"), stringArg(p.Arguments, "reason"))
		if err != nil {
			fail(err)
			return
		}
		toolResult(w, req.ID, map[string]any{"todo": todoJSON(t, base)})

	case "todo_scopes":
		scopes, err := h.Store.Scopes()
		if err != nil {
			fail(err)
			return
		}
		out := make([]map[string]any, 0, len(scopes))
		for _, sc := range scopes {
			out = append(out, map[string]any{"scope": sc.Scope, "open": sc.Open})
		}
		toolResult(w, req.ID, map[string]any{"scopes": out})
	}
}

func todoJSON(t store.Todo, base string) map[string]any {
	m := map[string]any{
		"id":         t.ID,
		"url":        link.Item(base, t.ID),
		"title":      t.Title,
		"body":       t.Body,
		"scope":      t.Scope,
		"source":     t.Source,
		"via":        t.Via,
		"state":      t.State,
		"created_at": t.CreatedAt,
		"updated_at": t.UpdatedAt,
	}
	if t.ClosedAt != "" {
		m["closed_at"] = t.ClosedAt
	}
	return m
}
