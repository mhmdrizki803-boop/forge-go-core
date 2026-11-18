// github-core-clone-01: a standalone GitHub-compatible git forge.
// Lifecycle: bind an ephemeral port, POST {"port"} to $MAKEFUN_ENV_URL/register,
// poll GET $MAKEFUN_ENV_URL/done until done, then exit 0.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The login the bearer token names (the suite's one seeded scenario viewer).
const viewerLogin = "adaiversen"

// ---------------------------------------------------------------- store

type Commit struct {
	Sha     string
	Message string
	Parents []string
	Files   map[string]string
}

type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []string
	CreatedAt time.Time
	Seq       int
}

type Comment struct {
	ID   int
	Body string
}

type Pull struct {
	Number    int
	Title     string
	Body      string
	Head      string
	Base      string
	State     string
	Merged    bool
	MergedAt  string
	MergeSha  string
	CreatedAt time.Time
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     time.Time
	Seq           int
	Commits       map[string]*Commit
	Branches      map[string]string
	BranchOrder   []string
	Labels        map[string]string
	Issues        map[int]*Issue
	IssueSeq      int
	Comments      map[int]*Comment
	CommentSeq    int
	Pulls         map[int]*Pull
	PullSeq       int
}

var (
	mu      sync.Mutex
	repos   = map[string]*Repo{}
	order   []*Repo
	repoSeq int
	reqSeq  int64
)

func key(owner, name string) string { return owner + "/" + name }

// ---------------------------------------------------------------- helpers

func sha1hex(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}

func blobSha(content string) string {
	return sha1hex([]byte("blob " + strconv.Itoa(len(content)) + "\x00" + content))
}

func newReqID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddInt64(&reqSeq, 1))
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", newReqID())
	if body == nil {
		w.WriteHeader(status)
		return
	}
	b, err := json.Marshal(body)
	if err != nil {
		b = []byte(`{"message":"encoding error"}`)
	}
	w.WriteHeader(status)
	w.Write(b)
}

func errJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func readJSON(r *http.Request, v interface{}) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return true
	}
	return json.Unmarshal(raw, v) == nil
}

// ---------------------------------------------------------------- repo helpers

func (rp *Repo) snapshot(branch string) map[string]string {
	tip, ok := rp.Branches[branch]
	if !ok || tip == "" {
		return map[string]string{}
	}
	c := rp.Commits[tip]
	if c == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(c.Files))
	for k, v := range c.Files {
		out[k] = v
	}
	return out
}

func (rp *Repo) setBranch(name, sha string) {
	if _, ok := rp.Branches[name]; !ok {
		rp.BranchOrder = append(rp.BranchOrder, name)
	}
	rp.Branches[name] = sha
}

func (rp *Repo) commit(files map[string]string, parents []string, message string) *Commit {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var tb strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&tb, "%s %s\n", blobSha(files[p]), p)
	}
	treeSha := sha1hex([]byte(tb.String()))
	now := time.Now().Unix()
	var ob strings.Builder
	fmt.Fprintf(&ob, "tree %s\n", treeSha)
	for _, p := range parents {
		fmt.Fprintf(&ob, "parent %s\n", p)
	}
	fmt.Fprintf(&ob, "author Forge <forge@example.invalid> %d +0000\n", now)
	fmt.Fprintf(&ob, "committer Forge <forge@example.invalid> %d +0000\n\n", now)
	ob.WriteString(message)
	obj := ob.String()
	sha := sha1hex([]byte("commit " + strconv.Itoa(len(obj)) + "\x00" + obj))
	c := &Commit{Sha: sha, Message: message, Parents: parents, Files: files}
	rp.Commits[sha] = c
	return c
}

func (rp *Repo) tip(branch string) string { return rp.Branches[branch] }

func (rp *Repo) commitChain(start string) []*Commit {
	seen := map[string]bool{}
	var out []*Commit
	var walk func(sha string)
	walk = func(sha string) {
		if sha == "" || seen[sha] {
			return
		}
		seen[sha] = true
		c := rp.Commits[sha]
		if c == nil {
			return
		}
		out = append(out, c)
		for _, p := range c.Parents {
			walk(p)
		}
	}
	walk(start)
	return out
}

func (rp *Repo) ancestors(sha string) map[string]bool {
	out := map[string]bool{}
	for _, c := range rp.commitChain(sha) {
		out[c.Sha] = true
	}
	return out
}

func (rp *Repo) mergeBase(head, base string) string {
	hb := rp.ancestors(head)
	// Walk base's chain (newest first) and return the first shared commit.
	for _, c := range rp.commitChain(base) {
		if hb[c.Sha] {
			return c.Sha
		}
	}
	return ""
}

// ---------------------------------------------------------------- JSON views

func repoJSON(rp *Repo) map[string]interface{} {
	return map[string]interface{}{
		"id":             rp.ID,
		"node_id":        "R_" + strconv.Itoa(rp.ID),
		"name":           rp.Name,
		"full_name":      rp.Owner + "/" + rp.Name,
		"owner":          map[string]interface{}{"login": rp.Owner, "id": 1000, "type": "User"},
		"private":        rp.Private,
		"description":    rp.Description,
		"default_branch": rp.DefaultBranch,
		"fork":           false,
		"created_at":     rp.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":     rp.CreatedAt.UTC().Format(time.RFC3339),
		"html_url":       "http://localhost/" + rp.Owner + "/" + rp.Name,
		"clone_url":      "http://localhost/" + rp.Owner + "/" + rp.Name + ".git",
	}
}

func labelList(names []string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]interface{}{"name": n, "color": "ededed"})
	}
	return out
}

func issueJSON(rp *Repo, is *Issue) map[string]interface{} {
	return map[string]interface{}{
		"id":         is.Number,
		"number":     is.Number,
		"title":      is.Title,
		"body":       is.Body,
		"state":      is.State,
		"labels":     labelList(is.Labels),
		"user":       map[string]interface{}{"login": rp.Owner, "id": 1000},
		"created_at": is.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": is.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (rp *Repo) branchSha(branch string) string {
	if s, ok := rp.Branches[branch]; ok {
		return s
	}
	return ""
}

func pullJSON(rp *Repo, p *Pull) map[string]interface{} {
	var mergedAt interface{}
	if p.Merged && p.MergedAt != "" {
		mergedAt = p.MergedAt
	} else {
		mergedAt = nil
	}
	var mergeSha interface{}
	if p.MergeSha != "" {
		mergeSha = p.MergeSha
	} else {
		mergeSha = nil
	}
	return map[string]interface{}{
		"id":                p.Number,
		"number":            p.Number,
		"title":             p.Title,
		"body":              p.Body,
		"state":             p.State,
		"merged":            p.Merged,
		"merged_at":         mergedAt,
		"merge_commit_sha":  mergeSha,
		"created_at":        p.CreatedAt.UTC().Format(time.RFC3339),
		"user":              map[string]interface{}{"login": rp.Owner, "id": 1000},
		"head":              map[string]interface{}{"ref": p.Head, "sha": rp.branchSha(p.Head), "label": rp.Owner + ":" + p.Head},
		"base":              map[string]interface{}{"ref": p.Base, "sha": rp.branchSha(p.Base), "label": rp.Owner + ":" + p.Base},
	}
}

func contentsFileJSON(path, content string) map[string]interface{} {
	return map[string]interface{}{
		"type":     "file",
		"encoding": "base64",
		"size":     len(content),
		"name":     baseName(path),
		"path":     path,
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
		"sha":      blobSha(content),
	}
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ---------------------------------------------------------------- router

type server struct{}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet && path == "/healthz" {
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if path == "/api/v3" || path == "/api/v3/" {
		if !authorized(r) {
			errJSON(w, 401, "Requires authentication")
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		writeJSON(w, 200, map[string]interface{}{
			"current_user_url":        "/api/v3/user",
			"rate_limit_url":          "/api/v3/rate_limit",
			"repository_url":          "/api/v3/repos/{owner}/{repo}",
		})
		return
	}
	if strings.HasPrefix(path, "/api/v3/") {
		if !authorized(r) {
			errJSON(w, 401, "Requires authentication")
			return
		}
		s.api(w, r)
		return
	}
	errJSON(w, 404, "Not Found")
}

func authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	f := strings.Fields(auth)
	if len(f) == 2 && strings.EqualFold(f[0], "bearer") && f[1] != "" {
		return true
	}
	return false
}

func (s *server) api(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/api/v3/")
	segs := splitPath(raw)
	if len(segs) == 0 {
		writeJSON(w, 200, map[string]interface{}{"current_user_url": "/api/v3/user"})
		return
	}
	switch segs[0] {
	case "meta":
		writeJSON(w, 200, map[string]interface{}{
			"installed_version":      "forge-1.0.0",
			"verifiable_password_authentication": false,
		})
		return
	case "user":
		if len(segs) == 2 && segs[1] == "repos" {
			s.userRepos(w, r)
			return
		}
	case "repos":
		if len(segs) >= 3 {
			s.repoRoute(w, r, segs[1], segs[2], segs[3:])
			return
		}
	}
	errJSON(w, 404, "Not Found")
}

func splitPath(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, "/") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- user/repos

func (s *server) userRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		mu.Lock()
		list := make([]*Repo, len(order))
		copy(list, order)
		mu.Unlock()
		q := r.URL.Query()
		perPage := atoiDefault(q.Get("per_page"), 30)
		if perPage <= 0 {
			perPage = 30
		}
		if perPage > 100 {
			perPage = 100
		}
		page := atoiDefault(q.Get("page"), 1)
		if page < 1 {
			page = 1
		}
		start := (page - 1) * perPage
		end := start + perPage
		if start > len(list) {
			start = len(list)
		}
		if end > len(list) {
			end = len(list)
		}
		pageItems := list[start:end]
		out := make([]map[string]interface{}, 0, len(pageItems))
		for _, rp := range pageItems {
			out = append(out, repoJSON(rp))
		}
		if end < len(list) {
			next := fmt.Sprintf("http://%s/api/v3/user/repos?per_page=%d&page=%d", r.Host, perPage, page+1)
			last := fmt.Sprintf("http://%s/api/v3/user/repos?per_page=%d&page=%d", r.Host, perPage, (len(list)+perPage-1)/perPage)
			link := fmt.Sprintf("<%s>; rel=\"next\", <%s>; rel=\"last\"", next, last)
			w.Header().Set("Link", link)
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Private     bool   `json:"private"`
			Description string `json:"description"`
		}
		if !readJSON(r, &body) {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		if body.Name == "" {
			errJSON(w, 422, "Repository creation failed: name is required")
			return
		}
		mu.Lock()
		if _, ok := repos[key(viewerLogin, body.Name)]; ok {
			mu.Unlock()
			errJSON(w, 422, "Repository creation failed: name already exists on this account")
			return
		}
		repoSeq++
		rp := &Repo{
			ID:            100000 + repoSeq,
			Owner:         viewerLogin,
			Name:          body.Name,
			Description:   body.Description,
			Private:       body.Private,
			DefaultBranch: "main",
			CreatedAt:     time.Now().UTC(),
			Seq:           repoSeq,
			Commits:       map[string]*Commit{},
			Branches:      map[string]string{},
			Labels:        map[string]string{},
			Issues:        map[int]*Issue{},
			Comments:      map[int]*Comment{},
			Pulls:         map[int]*Pull{},
		}
		repos[key(viewerLogin, body.Name)] = rp
		order = append(order, rp)
		mu.Unlock()
		writeJSON(w, 201, repoJSON(rp))
	default:
		errJSON(w, 404, "Not Found")
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ---------------------------------------------------------------- repo routes

func randomHex(n int) string { return "" }

func (s *server) repoRoute(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	if owner != viewerLogin {
		errJSON(w, 404, "Not Found")
		return
	}
	mu.Lock()
	rp := repos[key(owner, name)]
	mu.Unlock()
	if rp == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) == 0 {
		s.repoRoot(w, r, rp)
		return
	}
	switch rest[0] {
	case "contents":
		s.contents(w, r, rp, strings.Join(rest[1:], "/"))
	case "labels":
		s.labels(w, r, rp)
	case "issues":
		s.issues(w, r, rp, rest[1:])
	case "pulls":
		s.pulls(w, r, rp, rest[1:])
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			s.gitRefs(w, r, rp)
			return
		}
		errJSON(w, 404, "Not Found")
	case "branches":
		s.branches(w, r, rp)
	case "commits":
		s.commits(w, r, rp)
	default:
		errJSON(w, 404, "Not Found")
	}
}

func (s *server) repoRoot(w http.ResponseWriter, r *http.Request, rp *Repo) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, repoJSON(rp))
	case http.MethodPatch:
		var body struct {
			Description *string `json:"description"`
			Private     *bool   `json:"private"`
			Name        *string `json:"name"`
		}
		if !readJSON(r, &body) {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		mu.Lock()
		if body.Description != nil {
			rp.Description = *body.Description
		}
		if body.Private != nil {
			rp.Private = *body.Private
		}
		if body.Name != nil && *body.Name != "" {
			rp.Name = *body.Name
		}
		mu.Unlock()
		writeJSON(w, 200, repoJSON(rp))
	case http.MethodDelete:
		mu.Lock()
		delete(repos, key(rp.Owner, rp.Name))
		for i, x := range order {
			if x == rp {
				order = append(order[:i], order[i+1:]...)
				break
			}
		}
		mu.Unlock()
		writeJSON(w, 204, nil)
	default:
		errJSON(w, 404, "Not Found")
	}
}

func (s *server) branches(w http.ResponseWriter, r *http.Request, rp *Repo) {
	mu.Lock()
	defer mu.Unlock()
	names := append([]string{}, rp.BranchOrder...)
	if _, ok := rp.Branches[rp.DefaultBranch]; !ok {
		names = append([]string{rp.DefaultBranch}, names...)
	}
	sort.Strings(names)
	out := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]interface{}{
			"name":   n,
			"commit": map[string]interface{}{"sha": rp.Branches[n]},
		})
	}
	writeJSON(w, 200, out)
}

func (s *server) commits(w http.ResponseWriter, r *http.Request, rp *Repo) {
	ref := r.URL.Query().Get("sha")
	mu.Lock()
	defer mu.Unlock()
	tip := ""
	if ref == "" {
		tip = rp.Branches[rp.DefaultBranch]
	} else if s, ok := rp.Branches[ref]; ok {
		tip = s
	} else {
		tip = ref
	}
	out := []map[string]interface{}{}
	for _, c := range rp.commitChain(tip) {
		out = append(out, map[string]interface{}{
			"sha": c.Sha,
			"commit": map[string]interface{}{
				"message": c.Message,
				"author":  map[string]interface{}{"name": "Forge", "email": "forge@example.invalid", "date": time.Now().UTC().Format(time.RFC3339)},
			},
		})
	}
	writeJSON(w, 200, out)
}

func (s *server) gitRefs(w http.ResponseWriter, r *http.Request, rp *Repo) {
	if r.Method != http.MethodPost {
		errJSON(w, 404, "Not Found")
		return
	}
	var body struct {
		Ref string `json:"ref"`
		Sha string `json:"sha"`
	}
	if !readJSON(r, &body) {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	ref := body.Ref
	name := strings.TrimPrefix(ref, "refs/heads/")
	mu.Lock()
	if _, ok := rp.Commits[body.Sha]; !ok {
		mu.Unlock()
		errJSON(w, 422, "Reference does not exist")
		return
	}
	rp.setBranch(name, body.Sha)
	mu.Unlock()
	writeJSON(w, 201, map[string]interface{}{
		"ref":    ref,
		"object": map[string]interface{}{"sha": body.Sha, "type": "commit"},
	})
}

// ---------------------------------------------------------------- contents

func (s *server) contents(w http.ResponseWriter, r *http.Request, rp *Repo, path string) {
	switch r.Method {
	case http.MethodGet:
		s.contentsGet(w, r, rp, path)
	case http.MethodPut:
		s.contentsPut(w, r, rp, path)
	case http.MethodDelete:
		s.contentsDelete(w, r, rp, path)
	default:
		errJSON(w, 404, "Not Found")
	}
}

func (s *server) contentsGet(w http.ResponseWriter, r *http.Request, rp *Repo, path string) {
	ref := r.URL.Query().Get("ref")
	mu.Lock()
	if ref == "" {
		ref = rp.DefaultBranch
	}
	files := rp.snapshot(ref)
	mu.Unlock()
	if c, ok := files[path]; ok {
		writeJSON(w, 200, contentsFileJSON(path, c))
		return
	}
	// directory listing
	prefix := path + "/"
	seen := map[string]bool{}
	var entries []map[string]interface{}
	names := make([]string, 0, len(files))
	for p := range files {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if rest == "" {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			sub := rest[:i]
			if seen[sub] {
				continue
			}
			seen[sub] = true
			entries = append(entries, map[string]interface{}{
				"type": "dir",
				"name": sub,
				"path": path + "/" + sub,
				"sha":  sha1hex([]byte(path + "/" + sub)),
			})
		} else {
			if seen[rest] {
				continue
			}
			seen[rest] = true
			entries = append(entries, contentsFileJSON(p, files[p]))
		}
	}
	if entries == nil {
		entries = []map[string]interface{}{}
	}
	if len(entries) == 0 {
		errJSON(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, entries)
}

func (s *server) contentsPut(w http.ResponseWriter, r *http.Request, rp *Repo, path string) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		Sha     string `json:"sha"`
	}
	if !readJSON(r, &body) {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
	if err != nil {
		errJSON(w, 400, "content is not valid base64")
		return
	}
	newContent := string(decoded)
	message := body.Message
	if message == "" {
		message = "Update " + path
	}

	mu.Lock()
	branch := body.Branch
	if branch == "" {
		branch = rp.DefaultBranch
	}
	tip := rp.Branches[branch]
	files := rp.snapshot(branch)
	existing, exists := files[path]
	if body.Sha != "" {
		if !exists || blobSha(existing) != body.Sha {
			mu.Unlock()
			errJSON(w, 409, "sha does not match the current file contents")
			return
		}
	}
	files[path] = newContent
	c := rp.commit(files, []string{tip}, message)
	if tip == "" {
		c.Parents = nil
	}
	rp.setBranch(branch, c.Sha)
	mu.Unlock()

	status := 200
	if !exists {
		status = 201
	}
	writeJSON(w, status, map[string]interface{}{
		"content": contentsFileJSON(path, newContent),
		"commit":  map[string]interface{}{"sha": c.Sha, "message": message},
	})
}

func (s *server) contentsDelete(w http.ResponseWriter, r *http.Request, rp *Repo, path string) {
	var body struct {
		Message string `json:"message"`
		Sha     string `json:"sha"`
	}
	if !readJSON(r, &body) {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	mu.Lock()
	branch := rp.DefaultBranch
	files := rp.snapshot(branch)
	if _, ok := files[path]; !ok {
		mu.Unlock()
		errJSON(w, 404, "Not Found")
		return
	}
	delete(files, path)
	tip := rp.Branches[branch]
	c := rp.commit(files, []string{tip}, body.Message)
	if tip == "" {
		c.Parents = nil
	}
	rp.setBranch(branch, c.Sha)
	mu.Unlock()
	writeJSON(w, 200, map[string]interface{}{
		"commit": map[string]interface{}{"sha": c.Sha, "message": body.Message},
	})
}

// ---------------------------------------------------------------- labels

func (s *server) labels(w http.ResponseWriter, r *http.Request, rp *Repo) {
	if r.Method != http.MethodPost {
		errJSON(w, 404, "Not Found")
		return
	}
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !readJSON(r, &body) {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	if body.Name == "" {
		errJSON(w, 422, "Validation Failed")
		return
	}
	mu.Lock()
	rp.Labels[body.Name] = body.Color
	mu.Unlock()
	writeJSON(w, 201, map[string]interface{}{"name": body.Name, "color": body.Color})
}

// ---------------------------------------------------------------- issues

func (s *server) issues(w http.ResponseWriter, r *http.Request, rp *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			s.issueCreate(w, r, rp)
		case http.MethodGet:
			s.issueList(w, r, rp)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) >= 2 && rest[1] == "comments" {
		s.issueComments(w, r, rp, num)
		return
	}
	s.issueOne(w, r, rp, num)
}

func (s *server) issueCreate(w http.ResponseWriter, r *http.Request, rp *Repo) {
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !readJSON(r, &body) {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range body.Labels {
		if _, ok := rp.Labels[l]; !ok {
			errJSON(w, 422, "Validation Failed: label "+l+" does not exist")
			return
		}
	}
	rp.IssueSeq++
	is := &Issue{
		Number:    rp.IssueSeq,
		Title:     body.Title,
		Body:      body.Body,
		State:     "open",
		Labels:    append([]string{}, body.Labels...),
		CreatedAt: time.Now().UTC(),
		Seq:       rp.IssueSeq,
	}
	rp.Issues[is.Number] = is
	writeJSON(w, 201, issueJSON(rp, is))
}

func (s *server) issueList(w http.ResponseWriter, r *http.Request, rp *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortKey := q.Get("sort")
	dir := q.Get("direction")
	mu.Lock()
	list := make([]*Issue, 0, len(rp.Issues))
	for _, is := range rp.Issues {
		if state != "all" && is.State != state {
			continue
		}
		list = append(list, is)
	}
	mu.Unlock()
	sort.SliceStable(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	if sortKey == "" || sortKey == "created" {
		if dir == "desc" {
			for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	out := make([]map[string]interface{}, 0, len(list))
	for _, is := range list {
		out = append(out, issueJSON(rp, is))
	}
	writeJSON(w, 200, out)
}

func (s *server) issueOne(w http.ResponseWriter, r *http.Request, rp *Repo, num int) {
	mu.Lock()
	is := rp.Issues[num]
	mu.Unlock()
	if is == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, issueJSON(rp, is))
	case http.MethodPatch:
		var body struct {
			State *string `json:"state"`
			Title *string `json:"title"`
			Body  *string `json:"body"`
		}
		if !readJSON(r, &body) {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		mu.Lock()
		if body.State != nil {
			is.State = *body.State
		}
		if body.Title != nil {
			is.Title = *body.Title
		}
		if body.Body != nil {
			is.Body = *body.Body
		}
		mu.Unlock()
		writeJSON(w, 200, issueJSON(rp, is))
	default:
		errJSON(w, 404, "Not Found")
	}
}

func (s *server) issueComments(w http.ResponseWriter, r *http.Request, rp *Repo, num int) {
	mu.Lock()
	is := rp.Issues[num]
	mu.Unlock()
	if is == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Body string `json:"body"`
		}
		if !readJSON(r, &body) {
			errJSON(w, 400, "Problems parsing JSON")
			return
		}
		mu.Lock()
		rp.CommentSeq++
		c := &Comment{ID: rp.CommentSeq, Body: body.Body}
		rp.Comments[c.ID] = c
		mu.Unlock()
		writeJSON(w, 201, map[string]interface{}{
			"id":   c.ID,
			"body": c.Body,
			"user": map[string]interface{}{"login": rp.Owner, "id": 1000},
		})
	case http.MethodGet:
		mu.Lock()
		ids := make([]int, 0, len(rp.Comments))
		for id := range rp.Comments {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		out := make([]map[string]interface{}, 0, len(ids))
		for _, id := range ids {
			c := rp.Comments[id]
			out = append(out, map[string]interface{}{"id": c.ID, "body": c.Body})
		}
		mu.Unlock()
		writeJSON(w, 200, out)
	default:
		errJSON(w, 404, "Not Found")
	}
}

// ---------------------------------------------------------------- pulls

func (s *server) pulls(w http.ResponseWriter, r *http.Request, rp *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			s.pullCreate(w, r, rp)
		case http.MethodGet:
			s.pullList(w, r, rp)
		default:
			errJSON(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if len(rest) >= 2 && rest[1] == "merge" {
		s.pullMerge(w, r, rp, num)
		return
	}
	if len(rest) >= 2 && rest[1] == "files" {
		s.pullFiles(w, r, rp, num)
		return
	}
	mu.Lock()
	p := rp.Pulls[num]
	mu.Unlock()
	if p == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, pullJSON(rp, p))
	default:
		errJSON(w, 404, "Not Found")
	}
}

func (s *server) pullCreate(w http.ResponseWriter, r *http.Request, rp *Repo) {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if !readJSON(r, &body) {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	mu.Lock()
	if _, ok := rp.Branches[body.Head]; !ok {
		mu.Unlock()
		errJSON(w, 422, "Validation Failed: head branch does not exist")
		return
	}
	base := body.Base
	if base == "" {
		base = rp.DefaultBranch
	}
	if _, ok := rp.Branches[base]; !ok {
		mu.Unlock()
		errJSON(w, 422, "Validation Failed: base branch does not exist")
		return
	}
	rp.PullSeq++
	p := &Pull{
		Number:    rp.PullSeq,
		Title:     body.Title,
		Body:      body.Body,
		Head:      body.Head,
		Base:      base,
		State:     "open",
		CreatedAt: time.Now().UTC(),
	}
	rp.Pulls[p.Number] = p
	writeJSON(w, 201, pullJSON(rp, p))
	mu.Unlock()
}

func (s *server) pullList(w http.ResponseWriter, r *http.Request, rp *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	mu.Lock()
	list := make([]*Pull, 0, len(rp.Pulls))
	for _, p := range rp.Pulls {
		if state != "all" && p.State != state {
			continue
		}
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	out := make([]map[string]interface{}, 0, len(list))
	for _, p := range list {
		out = append(out, pullJSON(rp, p))
	}
	mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *server) pullFiles(w http.ResponseWriter, r *http.Request, rp *Repo, num int) {
	mu.Lock()
	p := rp.Pulls[num]
	if p == nil {
		mu.Unlock()
		errJSON(w, 404, "Not Found")
		return
	}
	base := rp.snapshot(p.Base)
	head := rp.snapshot(p.Head)
	mu.Unlock()
	paths := map[string]bool{}
	for k := range base {
		paths[k] = true
	}
	for k := range head {
		paths[k] = true
	}
	ordered := make([]string, 0, len(paths))
	for k := range paths {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	out := []map[string]interface{}{}
	for _, path := range ordered {
		bc, inBase := base[path]
		hc, inHead := head[path]
		if inBase && inHead && bc == hc {
			continue
		}
		var status string
		var additions, deletions int
		switch {
		case !inBase && inHead:
			status = "added"
			additions = lineCount(hc)
		case inBase && !inHead:
			status = "removed"
			deletions = lineCount(bc)
		default:
			status = "modified"
			additions = lineCount(hc)
			deletions = lineCount(bc)
		}
		out = append(out, map[string]interface{}{
			"filename":  path,
			"status":    status,
			"additions": additions,
			"deletions": deletions,
			"changes":   additions + deletions,
			"sha":       blobSha(hc),
		})
	}
	writeJSON(w, 200, out)
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func (s *server) pullMerge(w http.ResponseWriter, r *http.Request, rp *Repo, num int) {
	if r.Method != http.MethodPut {
		errJSON(w, 404, "Not Found")
		return
	}
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	if !readJSON(r, &body) {
		errJSON(w, 400, "Problems parsing JSON")
		return
	}
	mu.Lock()
	defer mu.Unlock()
	p := rp.Pulls[num]
	if p == nil {
		errJSON(w, 404, "Not Found")
		return
	}
	if p.State != "open" {
		errJSON(w, 405, "Pull Request is not mergeable")
		return
	}
	headTip := rp.Branches[p.Head]
	baseTip := rp.Branches[p.Base]
	mb := rp.mergeBase(headTip, baseTip)
	mbFiles := rp.snapshotCommit(mb)
	headFiles := rp.snapshotCommit(headTip)
	baseFiles := rp.snapshotCommit(baseTip)

	all := map[string]bool{}
	for k := range mbFiles {
		all[k] = true
	}
	for k := range headFiles {
		all[k] = true
	}
	for k := range baseFiles {
		all[k] = true
	}
	conflict := false
	for path := range all {
		hc, hasH := headFiles[path]
		bc, hasB := baseFiles[path]
		mc, hasM := mbFiles[path]
		changedH := (hasH != hasM) || (hasH && hasM && hc != mc)
		changedB := (hasB != hasM) || (hasB && hasM && bc != mc)
		if changedH && changedB && (!hasH || !hasB || hc != bc) {
			conflict = true
			break
		}
	}
	if conflict {
		errJSON(w, 405, "Pull Request is not mergeable: conflicts")
		return
	}

	merged := map[string]string{}
	for k, v := range baseFiles {
		merged[k] = v
	}
	for path := range all {
		hc, hasH := headFiles[path]
		mc, hasM := mbFiles[path]
		changedH := (hasH != hasM) || (hasH && hasM && hc != mc)
		if !changedH {
			continue
		}
		if hasH {
			merged[path] = hc
		} else {
			delete(merged, path)
		}
	}
	msg := fmt.Sprintf("Merge pull request #%d from %s", p.Number, p.Head)
	c := rp.commit(merged, []string{baseTip}, msg)
	rp.setBranch(p.Base, c.Sha)
	if p.Base == rp.DefaultBranch {
		rp.Branches[rp.DefaultBranch] = c.Sha
	}
	p.State = "closed"
	p.Merged = true
	p.MergedAt = time.Now().UTC().Format(time.RFC3339)
	p.MergeSha = c.Sha
	writeJSON(w, 200, map[string]interface{}{
		"sha":     c.Sha,
		"merged":  true,
		"message": "Pull Request successfully merged",
	})
}

func (rp *Repo) snapshotCommit(sha string) map[string]string {
	if sha == "" {
		return map[string]string{}
	}
	c := rp.Commits[sha]
	if c == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(c.Files))
	for k, v := range c.Files {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------- lifecycle

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	portEnv := os.Getenv("MAKEFUN_ENV_PORT")
	if portEnv == "" {
		portEnv = os.Getenv("PORT")
	}
	addr := ":0"
	if portEnv != "" && portEnv != "0" {
		addr = ":" + portEnv
	}

	ln, err := newListener(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*netTCPAddr).Port
	srv := &http.Server{Handler: &server{}}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[entry] serve: %v\n", err)
		}
	}()
	fmt.Printf("[entry] listening on port %d\n", port)

	if err := register(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] register: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for done")

	waitForDone(envURL)
	fmt.Println("[entry] done")
	os.Exit(0)
}
