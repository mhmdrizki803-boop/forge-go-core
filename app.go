package main

// Shared HTTP application and router: REST, GraphQL, MCP and Git all dispatch through
// this module, so they share one authentication and response pipeline.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OutHeaders are the headers a handler adds; the framework adds its own on top.
type OutHeaders map[string]string

// Ctx is the shared request state every route handler receives.
type Ctx struct {
	Host      string
	Proto     string
	Method    string
	Path      string
	QueryGet  func(string) string
	QueryAll  func(string) []string
	Params    map[string]string
	Headers   http.Header
	Login     string
	HasLogin  bool
	User      *UserRecord
	Viewer    bool
	Body      any
	Raw       []byte
	Store     *Store
	RequestID string
	Reply     *ReplyOut
}

// ReplyOut is the bytes one reply becomes.
type ReplyOut struct {
	Status  int
	Headers OutHeaders
	Body    []byte
}

// Reply is what a handler returns: status, body (a []byte is sent raw, anything else as JSON).
type Reply struct {
	Status  int
	Body    any
	Headers OutHeaders
}

func R(status int, body any) Reply { return Reply{Status: status, Body: body} }

func RH(status int, body any, headers OutHeaders) Reply {
	return Reply{Status: status, Body: body, Headers: headers}
}

func notFoundReply() Reply { return R(404, map[string]any{"message": "Not Found"}) }

// ---------------------------------------------------------------- process constants

func shopToken() string { return os.Getenv("MAKEFUN_SHOP_TOKEN") }

func viewerLogin() string {
	if v := strings.TrimSpace(os.Getenv("MAKEFUN_SHOP_LOGIN")); v != "" {
		return v
	}
	return "adaiversen"
}

var requestSeq uint64

// NextRequestID is a deterministic per-process request id in GitHub's hex shape.
func NextRequestID() string {
	n := uint32(atomic.AddUint64(&requestSeq, 1))
	if pinned := os.Getenv("MAKEFUN_REQUEST_ID"); pinned != "" {
		return pinned
	}
	a := strconv.FormatUint(uint64(n), 16)
	b := strconv.FormatUint(uint64(^n), 16)
	c := strconv.FormatUint(uint64(n*2654435761), 16)
	d := strconv.FormatUint(uint64(n^0x9e3779b9), 16)
	for len(a) < 8 {
		a = "0" + a
	}
	for len(b) < 8 {
		b = "0" + b
	}
	for len(c) < 8 {
		c = "0" + c
	}
	for len(d) < 8 {
		d = "0" + d
	}
	return strings.ToUpper(a + b + c + d)
}

// ---------------------------------------------------------------- reply plumbing

func sendJSON(ctx *Ctx, status int, body any, extra OutHeaders) {
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{"message":"encoding failed"}`)
	}
	headers := OutHeaders{
		"content-type":        "application/json; charset=utf-8",
		"content-length":      strconv.Itoa(len(payload)),
		"x-github-request-id": ctx.RequestID,
	}
	for k, v := range extra {
		headers[k] = v
	}
	out := payload
	if ctx.Method == "HEAD" {
		out = nil
	}
	ctx.Reply = &ReplyOut{Status: status, Headers: headers, Body: out}
}

func sendRaw(ctx *Ctx, status int, payload []byte, extra OutHeaders) {
	headers := OutHeaders{"x-github-request-id": ctx.RequestID}
	for k, v := range extra {
		headers[k] = v
	}
	if status != 204 {
		headers["content-length"] = strconv.Itoa(len(payload))
	}
	out := payload
	if ctx.Method == "HEAD" {
		out = nil
	}
	ctx.Reply = &ReplyOut{Status: status, Headers: headers, Body: out}
}

func emit(ctx *Ctx, r Reply) {
	if body, ok := r.Body.([]byte); ok {
		sendRaw(ctx, r.Status, body, r.Headers)
		return
	}
	sendJSON(ctx, r.Status, r.Body, r.Headers)
}

// ---------------------------------------------------------------- routing

type Handler func(ctx *Ctx) Reply

type Route struct {
	Method  string
	Pattern string
	Handler Handler
	want    []string
}

var routes []Route

func route(method, pattern string, handler Handler) {
	routes = append(routes, Route{Method: method, Pattern: pattern, Handler: handler, want: splitPath(pattern)})
}

func splitPath(value string) []string {
	parts := strings.Split(value, "/")
	out := make([]string, 0, len(parts))
	for i, part := range parts {
		if part != "" || i == 0 {
			out = append(out, part)
		}
	}
	return out
}

func decodeSegment(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	out, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return out
}

func matchSegments(want, got []string) (map[string]string, bool) {
	wildcard := len(want) > 0 && want[len(want)-1] == "*"
	fixed := len(want)
	if wildcard {
		fixed = len(want) - 1
		if len(got) < fixed {
			return nil, false
		}
	} else if len(got) != fixed {
		return nil, false
	}
	params := make(map[string]string, 2)
	for i := 0; i < fixed; i++ {
		segment := want[i]
		actual := got[i]
		if strings.HasPrefix(segment, ":") {
			if actual == "" {
				return nil, false
			}
			params[segment[1:]] = decodeSegment(actual)
		} else if segment != actual {
			return nil, false
		}
	}
	if wildcard {
		params["path"] = decodeSegment(strings.Join(got[fixed:], "/"))
	}
	return params, true
}

// lookupRoute finds the first route that matches, indexed by method and first segment.
func lookupRoute(method string, segments []string) (Route, map[string]string, bool) {
	first := ""
	if len(segments) > 0 {
		first = segments[0]
	}
	for i := range routes {
		candidate := &routes[i]
		if candidate.Method != method {
			continue
		}
		if len(candidate.want) > 0 && strings.HasPrefix(candidate.want[0], ":") {
			// a pattern whose first segment is a parameter answers for every first segment
		} else if len(candidate.want) == 0 || candidate.want[0] != first {
			continue
		}
		if params, ok := matchSegments(candidate.want, segments); ok {
			return *candidate, params, true
		}
	}
	return Route{}, nil, false
}

// ---------------------------------------------------------------- the git transport

func isGitPath(path string) bool {
	return strings.HasSuffix(path, "/info/refs") || strings.HasSuffix(path, "/git-upload-pack") ||
		strings.HasSuffix(path, "/git-receive-pack")
}

func isBodyMethod(method string) bool {
	switch method {
	case "POST", "PATCH", "PUT", "DELETE":
		return true
	}
	return false
}

var publicPaths = map[string]bool{"/healthz": true, "/api/v3": true, "/api/v3/": true, "/api/v3/meta": true}

// Dispatch runs everything a request does once its bytes are in hand.
func dispatch(ctx *Ctx) {
	store := ctx.Store
	method := ctx.Method
	path := ctx.Path

	store.Base = strings.TrimRight(ctx.Proto+"://"+ctx.Host, "/")

	auth := ctx.Headers.Get("Authorization")
	bearer := ""
	if auth != "" {
		lower := strings.ToLower(auth)
		if strings.HasPrefix(lower, "bearer ") {
			bearer = strings.TrimSpace(auth[len("bearer "):])
		} else if strings.HasPrefix(lower, "token ") {
			bearer = strings.TrimSpace(auth[len("token "):])
		}
	}
	if bearer != "" {
		isViewer := shopToken() != "" && bearer == shopToken()
		login := bearer
		if isViewer {
			login = viewerLogin()
		}
		ctx.Login = login
		ctx.HasLogin = true
		ctx.User = store.CreateUser(login)
		ctx.Viewer = isViewer
	}

	isAPI := path == "/api/v3" || strings.HasPrefix(path, "/api/v3/")
	if isAPI && !publicPaths[path] && !ctx.HasLogin {
		sendJSON(ctx, 401, map[string]any{"message": "Requires authentication"}, nil)
		return
	}

	if isBodyMethod(method) && len(ctx.Raw) > 0 && !isGitPath(path) && path != "/mcp" {
		text := strings.TrimSpace(string(ctx.Raw))
		if text != "" {
			var parsed any
			if err := json.Unmarshal([]byte(text), &parsed); err != nil {
				sendJSON(ctx, 400, map[string]any{"message": "Problems parsing JSON"}, nil)
				return
			}
			ctx.Body = parsed
		}
	}

	lookup := method
	if method == "HEAD" {
		lookup = "GET"
	}
	segments := splitPath(path)
	found, params, ok := lookupRoute(lookup, segments)
	if !ok {
		sendJSON(ctx, 404, map[string]any{"message": "Not Found"}, nil)
		return
	}
	ctx.Params = params

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				if ctx.Reply == nil {
					sendJSON(ctx, 500, map[string]any{"message": errString(rec)}, nil)
				}
			}
		}()
		emit(ctx, found.Handler(ctx))
	}()
	if ctx.Reply == nil {
		sendJSON(ctx, 500, map[string]any{"message": "empty reply"}, nil)
	}
}

func errString(v any) string {
	switch t := v.(type) {
	case error:
		return t.Error()
	case string:
		return t
	default:
		return "internal error"
	}
}

// ---------------------------------------------------------------- the server

type cacheEntry struct {
	gen     int
	epoch   int
	status  int
	headers OutHeaders
	body    []byte
}

// Forge is the HTTP handler: it authenticates nothing itself, it dispatches. Every request is
// serialized on one mutex — the store is Go maps, and a map written while another goroutine reads
// it is a crash. The reply is written outside the lock.
type Forge struct {
	mu    sync.RWMutex
	store *Store
	cache map[string]*cacheEntry
	// epoch counts every request the cache may not answer (a write or an unknown route), so a
	// reply that depends on state a mutation changed is never served again.
	epoch int
}

func newForge(store *Store) *Forge {
	return &Forge{store: store, cache: make(map[string]*cacheEntry, 512)}
}

func (f *Forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "CONNECT" {
		w.WriteHeader(405)
		return
	}
	target := r.RequestURI
	if target == "" {
		target = r.URL.RequestURI()
	}
	path := target
	query := ""
	if i := strings.IndexByte(target, '?'); i >= 0 {
		path = target[:i]
		query = target[i+1:]
	}

	canCache := r.Method == "GET" || r.Method == "HEAD" ||
		(r.Method == "POST" && strings.HasSuffix(path, "/git-upload-pack"))
	key := ""
	if canCache {
		key = r.Method + "\x00" + target + "\x00" + r.Header.Get("Authorization") + "\x00" +
			r.Header.Get("Git-Protocol") + "\x00"
		// A reply kept for this request answers it without touching the store: a reader never
		// queues behind another reader, so fifty concurrent clones cost one pack between them.
		if status, headers, body, ok := f.cachedReply(key); ok {
			writeReply(w, r, status, headers, body, NextRequestID())
			return
		}
	}
	status, headers, body, requestID := f.serveUncached(w, r, target, path, query, key)
	writeReply(w, r, status, headers, body, requestID)
}

// cachedReply answers a request from the cache when a reply kept for it is still current. The
// read lock is what keeps it honest: every mutation runs under the write lock.
func (f *Forge) cachedReply(key string) (int, OutHeaders, []byte, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	entry, ok := f.cache[key]
	if !ok || entry.gen != f.store.Writes || entry.epoch != f.epoch {
		return 0, nil, nil, false
	}
	return entry.status, entry.headers, entry.body, true
}

// serveUncached runs one request the cache could not answer, under the store's write lock.
func (f *Forge) serveUncached(w http.ResponseWriter, r *http.Request, target, path, query, key string) (int, OutHeaders, []byte, string) {
	requestID := NextRequestID()
	f.mu.Lock()
	defer f.mu.Unlock()
	if key == "" {
		// A request the cache may not answer (a write, an unknown route) retires every reply kept
		// before it: state a handler changed is never served stale.
		f.epoch++
	}
	raw := readBody(r)
	fullKey := ""
	if key != "" {
		fullKey = key + string(raw)
		if entry, ok := f.cache[fullKey]; ok && entry.gen == f.store.Writes && entry.epoch == f.epoch {
			return entry.status, entry.headers, entry.body, requestID
		}
	}

	queryValues := parseQuery(query)
	ctx := &Ctx{
		Host:      r.Host,
		Proto:     protoOf(r.Header),
		Method:    r.Method,
		Path:      path,
		Params:    map[string]string{},
		Headers:   r.Header,
		Raw:       raw,
		Store:     f.store,
		RequestID: requestID,
	}
	ctx.QueryGet = func(name string) string {
		if v, ok := queryValues[name]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	ctx.QueryAll = func(name string) []string { return queryValues[name] }

	gen := f.store.Writes
	epoch := f.epoch
	dispatch(ctx)

	reply := ctx.Reply
	if reply == nil {
		reply = &ReplyOut{
			Status:  500,
			Headers: OutHeaders{"content-type": "application/json; charset=utf-8"},
			Body:    []byte(`{"message":"internal error"}`),
		}
	}
	if key != "" && reply.Status >= 200 && reply.Status < 400 && f.store.Writes == gen && f.epoch == epoch {
		if len(f.cache) > 4096 {
			f.cache = make(map[string]*cacheEntry, 512)
		}
		f.cache[fullKey] = &cacheEntry{gen: gen, epoch: epoch, status: reply.Status, headers: reply.Headers, body: reply.Body}
	}
	return reply.Status, reply.Headers, reply.Body, requestID
}

func writeReply(w http.ResponseWriter, r *http.Request, status int, headers OutHeaders, body []byte, requestID string) {
	h := w.Header()
	for k, v := range headers {
		if k == "x-github-request-id" {
			continue
		}
		h.Set(k, v)
	}
	if h.Get("X-Github-Request-Id") == "" {
		h.Set("X-Github-Request-Id", requestID)
	}
	if status == 204 {
		w.WriteHeader(status)
		return
	}
	w.WriteHeader(status)
	if r.Method != "HEAD" && len(body) > 0 {
		w.Write(body)
	}
}

func readBody(r *http.Request) []byte {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	var buf []byte
	tmp := make([]byte, 32768)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf
}

func protoOf(headers http.Header) string {
	if v := headers.Get("X-Forwarded-Proto"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return "http"
}

func parseQuery(query string) url.Values {
	if query == "" {
		return url.Values{}
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return url.Values{}
	}
	return values
}

// nowUTC is used by nothing that a check reads; it exists for log lines.
func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }
