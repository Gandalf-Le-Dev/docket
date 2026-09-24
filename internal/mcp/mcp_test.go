package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/docket/internal/store"
)

type env struct {
	srv     *httptest.Server
	store   *store.Store
	publish string
	review  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	pub, err := s.CreateToken("box-1", "publish")
	if err != nil {
		t.Fatal(err)
	}
	rev, err := s.CreateToken("phone", "review")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(NewHandler(s, "test", ""))
	t.Cleanup(srv.Close)
	return &env{srv: srv, store: s, publish: pub, review: rev}
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

// call POSTs one JSON-RPC request. headers may be nil (legacy, headerless).
func (e *env) call(t *testing.T, token string, headers map[string]string, body string) (int, rpcResp) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out rpcResp
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// modern wraps arguments in a 2026-07-28-shaped tools/call with full headers.
func modernCall(name string, args string) (map[string]string, string) {
	headers := map[string]string{
		"MCP-Protocol-Version": ModernVersion,
		"Mcp-Method":           "tools/call",
		"Mcp-Name":             name,
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
		"name":%q,"arguments":%s,
		"_meta":{"io.modelcontextprotocol/protocolVersion":%q}}}`, name, args, ModernVersion)
	return headers, body
}

func structured(t *testing.T, r rpcResp) map[string]any {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("rpc error: %d %s", r.Error.Code, r.Error.Message)
	}
	var res struct {
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", res.Content[0].Text)
	}
	return res.StructuredContent
}

func toolErrText(t *testing.T, r rpcResp) string {
	t.Helper()
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(r.Result, &res)
	if !res.IsError {
		t.Fatalf("expected tool error, got %s", r.Result)
	}
	return res.Content[0].Text
}

func TestAuthRequired(t *testing.T) {
	e := newEnv(t)
	status, _ := e.call(t, "", nil, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	status, _ = e.call(t, "dkt_bogus", nil, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", status)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	e := newEnv(t)
	for _, method := range []string{"GET", "DELETE"} {
		req, _ := http.NewRequest(method, e.srv.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+e.review)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s: %d", method, resp.StatusCode)
		}
	}
}

func TestLegacyHandshake(t *testing.T) {
	e := newEnv(t)

	// Headerless initialize, as a 2025-03-26 client would send it.
	status, r := e.call(t, e.publish, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	if status != http.StatusOK || r.Error != nil {
		t.Fatalf("initialize: %d %+v", status, r.Error)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	json.Unmarshal(r.Result, &init)
	if init.ProtocolVersion != "2025-06-18" || init.ServerInfo.Name != "docket" || init.Instructions == "" {
		t.Fatalf("initialize result: %+v", init)
	}

	// An unsupported handshake version negotiates down to our newest legacy.
	_, r = e.call(t, e.publish, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	json.Unmarshal(r.Result, &init)
	if init.ProtocolVersion != "2025-06-18" {
		t.Fatalf("negotiation: %q", init.ProtocolVersion)
	}

	// notifications/initialized is a notification: 202, no body.
	status, _ = e.call(t, e.publish, nil, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if status != http.StatusAccepted {
		t.Fatalf("notification: %d", status)
	}
}

func TestRoleFiltersToolList(t *testing.T) {
	e := newEnv(t)
	names := func(token string) []string {
		_, r := e.call(t, token, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var res struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		json.Unmarshal(r.Result, &res)
		var out []string
		for _, tl := range res.Tools {
			out = append(out, tl.Name)
		}
		return out
	}

	pub := names(e.publish)
	if len(pub) != 1 || pub[0] != "todo_add" {
		t.Fatalf("publish sees: %v", pub)
	}
	rev := names(e.review)
	if len(rev) != 6 {
		t.Fatalf("review sees: %v", rev)
	}
}

// The 2026-07-28 revision requires resultType on every result, and clients of
// that revision reject results without it — absent-means-complete leniency
// applies only to servers speaking earlier revisions. Regression: tools/list
// lacked it, and a modern Claude Code refused the entire tool surface.
func TestResultsCarryResultType(t *testing.T) {
	e := newEnv(t)

	resultType := func(r rpcResp) string {
		var res struct {
			ResultType string `json:"resultType"`
		}
		json.Unmarshal(r.Result, &res)
		return res.ResultType
	}

	// Caching hints are mandatory on the cacheable results (server/discover
	// and tools/list) — and private, because both vary by the token's role.
	hints := func(r rpcResp) (int, string) {
		var res struct {
			TTLMs      int    `json:"ttlMs"`
			CacheScope string `json:"cacheScope"`
		}
		json.Unmarshal(r.Result, &res)
		return res.TTLMs, res.CacheScope
	}

	// tools/list, both eras served by the same handler.
	_, r := e.call(t, e.review, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if got := resultType(r); got != "complete" {
		t.Errorf("tools/list resultType = %q, want complete", got)
	}
	if ttl, scope := hints(r); ttl <= 0 || scope != "private" {
		t.Errorf("tools/list hints = %d %q, want positive ttl and private", ttl, scope)
	}

	// server/discover is the other cacheable result.
	_, r = e.call(t, e.review, nil, `{"jsonrpc":"2.0","id":9,"method":"server/discover"}`)
	if ttl, scope := hints(r); ttl <= 0 || scope != "private" {
		t.Errorf("server/discover hints = %d %q, want positive ttl and private", ttl, scope)
	}

	// ping.
	_, r = e.call(t, e.review, nil, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if got := resultType(r); got != "complete" {
		t.Errorf("ping resultType = %q, want complete", got)
	}

	// A successful modern tools/call.
	h, b := modernCall("todo_add", `{"title":"resultType check","source":"test"}`)
	_, r = e.call(t, e.publish, h, b)
	if got := resultType(r); got != "complete" {
		t.Errorf("tools/call resultType = %q, want complete", got)
	}

	// A tool execution error is still a result, and still carries it.
	h, b = modernCall("todo_add", `{"title":""}`)
	_, r = e.call(t, e.publish, h, b)
	if got := resultType(r); got != "complete" {
		t.Errorf("tool-error resultType = %q, want complete", got)
	}
}

func TestPublishCannotRead(t *testing.T) {
	e := newEnv(t)
	_, r := e.call(t, e.publish, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"todo_list","arguments":{}}}`)
	if r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Fatalf("publish read: %+v", r.Error)
	}
}

func TestAddListCloseRoundTrip(t *testing.T) {
	e := newEnv(t)

	// Publish files an item (legacy shape).
	_, r := e.call(t, e.publish, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"todo_add","arguments":{"title":"move setup onto hopbox","scope":"Personal","source":"repo:hopbox"}}}`)
	sc := structured(t, r)
	id := sc["id"].(float64)
	if sc["duplicate"].(bool) {
		t.Fatal("fresh add marked duplicate")
	}

	// Retry dedupes.
	_, r = e.call(t, e.publish, nil,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"todo_add","arguments":{"title":"move setup onto hopbox","scope":"personal"}}}`)
	sc = structured(t, r)
	if !sc["duplicate"].(bool) || sc["id"].(float64) != id {
		t.Fatalf("dedupe: %v", sc)
	}

	// Review lists it, with via recorded from the token, not the claim.
	_, r = e.call(t, e.review, nil,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"todo_list","arguments":{}}}`)
	sc = structured(t, r)
	todos := sc["todos"].([]any)
	if len(todos) != 1 {
		t.Fatalf("list: %v", todos)
	}
	item := todos[0].(map[string]any)
	if item["via"] != "box-1" || item["scope"] != "personal" || item["source"] != "repo:hopbox" {
		t.Fatalf("item: %v", item)
	}

	// Review closes it as dropped with a reason.
	_, r = e.call(t, e.review, nil, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"todo_close","arguments":{"id":%d,"outcome":"dropped","reason":"superseded"}}}`, int(id)))
	sc = structured(t, r)
	todo := sc["todo"].(map[string]any)
	if todo["state"] != "dropped" || todo["closed_at"] == nil {
		t.Fatalf("close: %v", todo)
	}

	// Scopes reflect only open items.
	_, r = e.call(t, e.review, nil,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"todo_scopes","arguments":{}}}`)
	sc = structured(t, r)
	if len(sc["scopes"].([]any)) != 0 {
		t.Fatalf("scopes after close: %v", sc)
	}
}

func TestToolErrorsAreToolErrors(t *testing.T) {
	e := newEnv(t)
	_, r := e.call(t, e.publish, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"todo_add","arguments":{"title":"   "}}}`)
	if msg := toolErrText(t, r); msg == "" {
		t.Fatal("empty tool error")
	}
	_, r = e.call(t, e.review, nil,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"todo_get","arguments":{"id":12345}}}`)
	if msg := toolErrText(t, r); msg != "no todo with that id" {
		t.Fatalf("not-found: %q", msg)
	}
}

func TestModernDiscoverAndCall(t *testing.T) {
	e := newEnv(t)

	// server/discover with modern headers.
	headers := map[string]string{
		"MCP-Protocol-Version": ModernVersion,
		"Mcp-Method":           "server/discover",
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"d1","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":%q}}}`, ModernVersion)
	status, r := e.call(t, e.review, headers, body)
	if status != http.StatusOK || r.Error != nil {
		t.Fatalf("discover: %d %+v", status, r.Error)
	}
	var disc struct {
		ResultType        string   `json:"resultType"`
		SupportedVersions []string `json:"supportedVersions"`
		Meta              map[string]struct {
			Name string `json:"name"`
		} `json:"_meta"`
	}
	json.Unmarshal(r.Result, &disc)
	if disc.ResultType != "complete" || disc.SupportedVersions[0] != ModernVersion {
		t.Fatalf("discover result: %+v", disc)
	}
	if disc.Meta["io.modelcontextprotocol/serverInfo"].Name != "docket" {
		t.Fatalf("serverInfo: %+v", disc.Meta)
	}

	// A modern tools/call with correct mirrored headers works end to end.
	h, b := modernCall("todo_add", `{"title":"modern era item","source":"test"}`)
	status, r = e.call(t, e.publish, h, b)
	if status != http.StatusOK {
		t.Fatalf("modern add: %d", status)
	}
	sc := structured(t, r)
	if sc["id"].(float64) < 1 {
		t.Fatalf("modern add result: %v", sc)
	}
}

func TestModernHeaderValidation(t *testing.T) {
	e := newEnv(t)

	// Mcp-Name mismatching the body is a 400 HeaderMismatch.
	h, b := modernCall("todo_add", `{"title":"x"}`)
	h["Mcp-Name"] = "something_else"
	status, r := e.call(t, e.publish, h, b)
	if status != http.StatusBadRequest || r.Error == nil || r.Error.Code != codeHeaderMismatch {
		t.Fatalf("name mismatch: %d %+v", status, r.Error)
	}

	// Missing Mcp-Method is likewise rejected.
	h, b = modernCall("todo_add", `{"title":"x"}`)
	delete(h, "Mcp-Method")
	status, r = e.call(t, e.publish, h, b)
	if status != http.StatusBadRequest || r.Error == nil || r.Error.Code != codeHeaderMismatch {
		t.Fatalf("missing method: %d %+v", status, r.Error)
	}

	// Header version without the matching _meta version is rejected.
	headers := map[string]string{
		"MCP-Protocol-Version": ModernVersion,
		"Mcp-Method":           "ping",
	}
	status, r = e.call(t, e.publish, headers, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`)
	if status != http.StatusBadRequest || r.Error == nil || r.Error.Code != codeHeaderMismatch {
		t.Fatalf("meta mismatch: %d %+v", status, r.Error)
	}

	// A Base64-sentinel-encoded Mcp-Name that decodes to the body value passes.
	h, b = modernCall("todo_add", `{"title":"sentinel"}`)
	h["Mcp-Name"] = "=?base64?dG9kb19hZGQ=?=" // "todo_add"
	status, r = e.call(t, e.publish, h, b)
	if status != http.StatusOK {
		t.Fatalf("sentinel: %d %+v", status, r.Error)
	}
	structured(t, r)
}

func TestUnsupportedVersion(t *testing.T) {
	e := newEnv(t)
	headers := map[string]string{"MCP-Protocol-Version": "1900-01-01"}
	status, r := e.call(t, e.review, headers, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if status != http.StatusBadRequest || r.Error == nil || r.Error.Code != codeUnsupportedVersion {
		t.Fatalf("unsupported: %d %+v", status, r.Error)
	}
	var data struct {
		Supported []string `json:"supported"`
		Requested string   `json:"requested"`
	}
	json.Unmarshal(r.Error.Data, &data)
	if len(data.Supported) != len(SupportedVersions) || data.Requested != "1900-01-01" {
		t.Fatalf("error data: %+v", data)
	}
}

func TestUnknownMethodStatusByEra(t *testing.T) {
	e := newEnv(t)

	// Legacy era: JSON-RPC error rides a 200.
	status, r := e.call(t, e.review, nil, `{"jsonrpc":"2.0","id":1,"method":"no/such"}`)
	if status != http.StatusOK || r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Fatalf("legacy unknown: %d %+v", status, r.Error)
	}

	// Modern era: 404 so probes can distinguish "modern server" from "no MCP".
	headers := map[string]string{
		"MCP-Protocol-Version": ModernVersion,
		"Mcp-Method":           "no/such",
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"no/such","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":%q}}}`, ModernVersion)
	status, r = e.call(t, e.review, headers, body)
	if status != http.StatusNotFound || r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Fatalf("modern unknown: %d %+v", status, r.Error)
	}
}

func TestAddRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < AddRateLimit; i++ {
		_, r := e.call(t, e.publish, nil, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"todo_add","arguments":{"title":"item %d"}}}`, i))
		structured(t, r)
	}
	_, r := e.call(t, e.publish, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"todo_add","arguments":{"title":"one too many"}}}`)
	if msg := toolErrText(t, r); msg == "" {
		t.Fatal("rate limit not enforced")
	}

	// The review token has its own budget.
	_, r = e.call(t, e.review, nil,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"todo_add","arguments":{"title":"review add"}}}`)
	structured(t, r)
}

func TestItemURL(t *testing.T) {
	e := newEnv(t)

	// todo_add hands the link back at once, built on the request's own host.
	h, b := modernCall("todo_add", `{"title":"needs a link","source":"test"}`)
	_, r := e.call(t, e.publish, h, b)
	res := structured(t, r)
	if res["url"] != e.srv.URL+"/todo/1" {
		t.Fatalf("todo_add url = %v", res["url"])
	}

	// Every item result carries it too.
	h, b = modernCall("todo_get", `{"id":1}`)
	_, r = e.call(t, e.review, h, b)
	todo := structured(t, r)["todo"].(map[string]any)
	if todo["url"] != e.srv.URL+"/todo/1" {
		t.Fatalf("todo_get url = %v", todo["url"])
	}
	h, b = modernCall("todo_list", `{}`)
	_, r = e.call(t, e.review, h, b)
	first := structured(t, r)["todos"].([]any)[0].(map[string]any)
	if first["url"] != e.srv.URL+"/todo/1" {
		t.Fatalf("todo_list url = %v", first["url"])
	}

	// Behind a reverse proxy the link is the public one, not the loopback.
	h, b = modernCall("todo_get", `{"id":1}`)
	h["X-Forwarded-Proto"] = "https"
	h["X-Forwarded-Host"] = "docket.example.net"
	_, r = e.call(t, e.review, h, b)
	todo = structured(t, r)["todo"].(map[string]any)
	if todo["url"] != "https://docket.example.net/todo/1" {
		t.Fatalf("proxied url = %v", todo["url"])
	}
}

// An agent reading an item sees its images, not just their paths.
func TestGetIncludesImages(t *testing.T) {
	e := newEnv(t)
	pic := tinyPNG(t, 0)
	imgID, err := e.store.AddImage(pic)
	if err != nil {
		t.Fatal(err)
	}
	// referenced twice, alongside an id that does not exist
	body := fmt.Sprintf("broken:\n![image](/image/%d)\n![gone](/image/999)\nagain ![image](/image/%d)", imgID, imgID)
	id, _, err := e.store.AddTodo("with a screenshot", body, "", "test", "t")
	if err != nil {
		t.Fatal(err)
	}

	h, b := modernCall("todo_get", fmt.Sprintf(`{"id":%d}`, id))
	_, r := e.call(t, e.review, h, b)
	if structured(t, r)["todo"].(map[string]any)["body"] != body {
		t.Fatal("body changed")
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 2 || res.Content[0].Type != "text" {
		t.Fatalf("content: %+v", res.Content)
	}
	got := res.Content[1]
	if got.Type != "image" || got.MimeType != "image/png" || got.Data != base64.StdEncoding.EncodeToString(pic) {
		t.Fatalf("image block: %+v", got)
	}

	// The list stays text only.
	h, b = modernCall("todo_list", `{}`)
	_, r = e.call(t, e.review, h, b)
	res.Content = nil
	json.Unmarshal(r.Result, &res)
	if len(res.Content) != 1 {
		t.Fatalf("todo_list content: %d blocks", len(res.Content))
	}
}

// A body naming hundreds of images gets a handful of them and one note for
// the rest, not a block per id.
func TestGetBoundsManyImages(t *testing.T) {
	e := newEnv(t)
	var body string
	for i := 1; i <= maxImageBlocks+2; i++ {
		n, err := e.store.AddImage(tinyPNG(t, uint8(i)))
		if err != nil {
			t.Fatal(err)
		}
		body += fmt.Sprintf("![image](/image/%d)\n", n)
	}
	for n := 1000; n < 1200; n++ {
		body += fmt.Sprintf("![image](/image/%d)", n)
	}
	id, _, err := e.store.AddTodo("many screenshots", body, "", "test", "t")
	if err != nil {
		t.Fatal(err)
	}
	h, b := modernCall("todo_get", fmt.Sprintf(`{"id":%d}`, id))
	_, r := e.call(t, e.review, h, b)
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1+maxImageBlocks+1 {
		t.Fatalf("todo_get with many images: %d blocks", len(res.Content))
	}
	for i, c := range res.Content[1 : 1+maxImageBlocks] {
		if c.Type != "image" {
			t.Fatalf("block %d is %s", i+1, c.Type)
		}
	}
	note := res.Content[len(res.Content)-1]
	if note.Type != "text" || !strings.HasPrefix(note.Text, "202 images left out") || !strings.HasSuffix(note.Text, "/image/1199") {
		t.Fatalf("closing note: %q", note.Text)
	}
}

// Images too heavy for one result are named instead of sent, whether one is
// too big alone or the result already carries enough.
func TestGetBoundsImageBytes(t *testing.T) {
	e := newEnv(t)
	var body string
	var ids []int64
	for i, rows := range []int{850, 1050, 850, 850} { // about 3.4, 4.2, 3.4 and 3.4 MB
		pic := flatPNG(t, 1000, rows, uint8(i+1))
		id, err := e.store.AddImage(pic)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		body += fmt.Sprintf("![image](/image/%d)\n", id)
	}
	id, _, err := e.store.AddTodo("heavy screenshots", body, "", "test", "t")
	if err != nil {
		t.Fatal(err)
	}

	h, b := modernCall("todo_get", fmt.Sprintf(`{"id":%d}`, id))
	_, r := e.call(t, e.review, h, b)
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Data string `json:"data"`
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	want := []string{"text", "image", "image", "text"}
	if len(res.Content) != len(want) {
		t.Fatalf("got %d blocks, want %d", len(res.Content), len(want))
	}
	payload := 0
	for i, c := range res.Content {
		if c.Type != want[i] {
			t.Fatalf("block %d is %s, want %s", i, c.Type, want[i])
		}
		if len(c.Data) > maxImageBlockBytes {
			t.Fatalf("block %d carries %d bytes", i, len(c.Data))
		}
		payload += len(c.Data)
	}
	if payload > maxImagePayloadBytes {
		t.Fatalf("result carries %d bytes of images", payload)
	}
	if note, want := res.Content[3].Text, fmt.Sprintf("2 images left out to keep this result small: /image/%d, /image/%d", ids[1], ids[3]); note != want {
		t.Fatalf("closing note = %q, want %q", note, want)
	}
}

// flatPNG is an uncompressed PNG of one color, so its size is predictable.
func flatPNG(t *testing.T, w, h int, fill uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = fill
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tinyPNG(t *testing.T, red uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{red, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestItemURLConfigured(t *testing.T) {
	e := newEnv(t)
	pinned := httptest.NewServer(NewHandler(e.store, "test", "https://todo.example.org/"))
	t.Cleanup(pinned.Close)
	e.srv = pinned

	h, b := modernCall("todo_add", `{"title":"pinned","source":"test"}`)
	h["X-Forwarded-Host"] = "spoofed.example"
	_, r := e.call(t, e.publish, h, b)
	if got := structured(t, r)["url"]; got != "https://todo.example.org/todo/1" {
		t.Fatalf("configured url = %v", got)
	}
}
