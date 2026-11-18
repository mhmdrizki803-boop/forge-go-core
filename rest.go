package main

// GitHub-compatible REST v3 routes for repositories, refs, commits, contents,
// labels, issues, comments and pull requests, backed by the shared store.

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func b64(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

// isoDate renders a git date ("<seconds>") as the ISO string the REST API prints.
func isoDate(seconds int64) string {
	if seconds == EpochSeconds {
		return EPOCH
	}
	return time.Unix(seconds, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func apiBase(ctx *Ctx) string { return ctx.Store.Base }

func repoURL(ctx *Ctx, repo *RepoRecord) string {
	return apiBase(ctx) + "/api/v3/repos/" + repo.FullName
}

func webURL(ctx *Ctx, repo *RepoRecord) string {
	return apiBase(ctx) + "/" + repo.FullName
}

func repoOf(ctx *Ctx) *RepoRecord {
	return ctx.Store.GetRepo(ctx.Params["owner"], ctx.Params["repo"])
}

var digitsOnly = regexp.MustCompile(`^\d+$`)

func numberOf(ctx *Ctx, name string) (int, bool) {
	raw, ok := ctx.Params[name]
	if !ok || !digitsOnly.MatchString(raw) {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

func asObject(ctx *Ctx) map[string]any {
	if obj, ok := ctx.Body.(map[string]any); ok {
		return obj
	}
	return map[string]any{}
}

func asString(value any) (string, bool) {
	if s, ok := value.(string); ok {
		return s, true
	}
	return "", false
}

func clampInt(raw string, fallback, low, high int) int {
	n, err := strconv.Atoi(raw)
	if raw == "" || err != nil {
		return fallback
	}
	if n < low {
		n = low
	}
	if n > high {
		n = high
	}
	return n
}

type authorID struct{ Name, Email string }

func authorFor(ctx *Ctx) authorID {
	if ctx.User == nil {
		return authorID{Name: "openforge", Email: "openforge@users.noreply.github.com"}
	}
	return authorID{Name: ctx.User.Name, Email: ctx.User.Email}
}

// ---------------------------------------------------------------- git objects: trees

const modeFile = "100644"
const modeDir = "40000"

type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

func readTree(store *Store, repo *RepoRecord, oid string) []TreeEntry {
	if oid == "" {
		return nil
	}
	object := store.GetObject(repo, oid)
	if object == nil || object.Type != "tree" {
		return nil
	}
	body := object.Body
	entries := []TreeEntry{}
	at := 0
	for at < len(body) {
		spaceRel := indexByteFrom(body, ' ', at)
		if spaceRel < 0 {
			break
		}
		nulRel := indexByteFrom(body, 0, spaceRel+1)
		if nulRel < 0 || nulRel+21 > len(body) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(body[at:spaceRel]),
			Name: string(body[spaceRel+1 : nulRel]),
			Oid:  hexEncode(body[nulRel+1 : nulRel+21]),
		})
		at = nulRel + 21
	}
	return entries
}

func indexByteFrom(body []byte, c byte, from int) int {
	for i := from; i < len(body); i++ {
		if body[i] == c {
			return i
		}
	}
	return -1
}

func encodeTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return treeSortKey(sorted[i]) < treeSortKey(sorted[j]) })
	out := []byte{}
	for _, entry := range sorted {
		out = append(out, []byte(entry.Mode+" "+entry.Name+"\x00")...)
		out = append(out, unhex(entry.Oid)...)
	}
	return out
}

func treeSortKey(entry TreeEntry) string {
	if entry.Mode == modeDir {
		return entry.Name + "/"
	}
	return entry.Name
}

func writeTree(store *Store, repo *RepoRecord, entries []TreeEntry) string {
	return store.PutObject(repo, "tree", encodeTree(entries))
}

func treeAt(store *Store, repo *RepoRecord, rootOid string, segments []string) string {
	current := rootOid
	for _, segment := range segments {
		var found *TreeEntry
		for _, entry := range readTree(store, repo, current) {
			if entry.Name == segment {
				e := entry
				found = &e
				break
			}
		}
		if found == nil || found.Mode != modeDir {
			return ""
		}
		current = found.Oid
	}
	return current
}

func fileAt(store *Store, repo *RepoRecord, rootOid, path string) (TreeEntry, bool) {
	segments := splitNonEmpty(path, "/")
	if len(segments) == 0 {
		return TreeEntry{}, false
	}
	parent := treeAt(store, repo, rootOid, segments[:len(segments)-1])
	name := segments[len(segments)-1]
	for _, entry := range readTree(store, repo, parent) {
		if entry.Name == name {
			return entry, true
		}
	}
	return TreeEntry{}, false
}

func splitNonEmpty(value, sep string) []string {
	out := []string{}
	for _, part := range strings.Split(value, sep) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// upsertPath returns the oid of a new tree with `name` at segments set to blobOid (or removed).
func upsertPath(store *Store, repo *RepoRecord, rootOid string, segments []string, blobOid string, remove bool) string {
	head := segments[0]
	rest := segments[1:]
	all := readTree(store, repo, rootOid)
	entries := make([]TreeEntry, 0, len(all)+1)
	for _, entry := range all {
		if entry.Name != head {
			entries = append(entries, entry)
		}
	}
	if len(rest) == 0 {
		if !remove {
			entries = append(entries, TreeEntry{Mode: modeFile, Name: head, Oid: blobOid})
		}
	} else {
		child := ""
		found := false
		for _, entry := range all {
			if entry.Name == head && entry.Mode == modeDir {
				child = entry.Oid
				found = true
				break
			}
		}
		_ = found
		child = upsertPath(store, repo, child, rest, blobOid, remove)
		if len(readTree(store, repo, child)) > 0 {
			entries = append(entries, TreeEntry{Mode: modeDir, Name: head, Oid: child})
		}
	}
	return writeTree(store, repo, entries)
}

func flattenTree(store *Store, repo *RepoRecord, oid, prefix string) map[string]TreeEntry {
	out := map[string]TreeEntry{}
	for _, entry := range readTree(store, repo, oid) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == modeDir {
			for childPath, child := range flattenTree(store, repo, entry.Oid, path) {
				out[childPath] = child
			}
		} else {
			out[path] = TreeEntry{Mode: entry.Mode, Name: path, Oid: entry.Oid}
		}
	}
	return out
}

// ---------------------------------------------------------------- git objects: commits

type Signature struct {
	Name  string
	Email string
	Date  string
}

type ParsedCommit struct {
	Tree      string
	Parents   []string
	Author    Signature
	Committer Signature
	Message   string
}

var signatureRe = regexp.MustCompile(`^(.*?) <([^>]*)> (\d+) ([+-]\d{4})$`)

func parseSignature(raw string) Signature {
	trimmed := strings.TrimSpace(raw)
	match := signatureRe.FindStringSubmatch(trimmed)
	if match == nil {
		return Signature{Name: trimmed}
	}
	secs, _ := strconv.ParseInt(match[3], 10, 64)
	return Signature{Name: match[1], Email: match[2], Date: isoDate(secs)}
}

func defaultSignature() Signature { return Signature{Date: EPOCH} }

func parseCommit(body []byte) ParsedCommit {
	raw := string(body)
	split := strings.Index(raw, "\n\n")
	header := raw
	message := ""
	if split >= 0 {
		header = raw[:split]
		message = strings.TrimRight(raw[split+2:], "\n")
	}
	out := ParsedCommit{Author: defaultSignature(), Committer: defaultSignature(), Message: message}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.Parents = append(out.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			out.Author = parseSignature(line[7:])
		case strings.HasPrefix(line, "committer "):
			out.Committer = parseSignature(line[10:])
		}
	}
	return out
}

func commitAt(store *Store, repo *RepoRecord, oid string) (ParsedCommit, bool) {
	if oid == "" {
		return ParsedCommit{}, false
	}
	object := store.GetObject(repo, oid)
	if object == nil || object.Type != "commit" {
		return ParsedCommit{}, false
	}
	return parseCommit(object.Body), true
}

func signatureJSON(sig Signature) map[string]any {
	return map[string]any{"name": sig.Name, "email": sig.Email, "date": sig.Date}
}

func commitJSON(ctx *Ctx, repo *RepoRecord, oid string) map[string]any {
	commit := ParsedCommit{Author: defaultSignature(), Committer: defaultSignature()}
	if object := ctx.Store.GetObject(repo, oid); object != nil && object.Type == "commit" {
		commit = parseCommit(object.Body)
	}
	parents := make([]any, 0, len(commit.Parents))
	for _, sha := range commit.Parents {
		parents = append(parents, map[string]any{"sha": sha, "url": repoURL(ctx, repo) + "/commits/" + sha})
	}
	return map[string]any{
		"sha":      oid,
		"node_id":  b64(oid),
		"url":      repoURL(ctx, repo) + "/commits/" + oid,
		"html_url": webURL(ctx, repo) + "/commit/" + oid,
		"commit": map[string]any{
			"message":   commit.Message,
			"author":    signatureJSON(commit.Author),
			"committer": signatureJSON(commit.Committer),
			"tree":      map[string]any{"sha": commit.Tree, "url": repoURL(ctx, repo) + "/git/trees/" + commit.Tree},
		},
		"parents": parents,
	}
}

func writeCommit(store *Store, repo *RepoRecord, tree string, parents []string, author authorID, message string) string {
	who := author.Name + " <" + author.Email + "> " + strconv.Itoa(EpochSeconds) + " +0000"
	var lines []string
	lines = append(lines, "tree "+tree)
	for _, parent := range parents {
		lines = append(lines, "parent "+parent)
	}
	lines = append(lines, "author "+who, "committer "+who)
	body := strings.Join(lines, "\n") + "\n\n" + strings.TrimRight(message, "\n") + "\n"
	return store.PutObject(repo, "commit", []byte(body))
}

func walkCommits(store *Store, repo *RepoRecord, oid string, limit int) []string {
	seen := map[string]bool{}
	order := []string{}
	queue := []string{}
	if oid != "" {
		queue = append(queue, oid)
	}
	for len(queue) > 0 && len(order) < limit {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true
		object := store.GetObject(repo, next)
		if object == nil || object.Type != "commit" {
			continue
		}
		order = append(order, next)
		queue = append(queue, parseCommit(object.Body).Parents...)
	}
	return order
}

func branchTip(store *Store, repo *RepoRecord, branch string) string {
	name := branch
	if name == "" {
		name = repo.DefaultBranch
	}
	return store.GetRef(repo, "refs/heads/"+name)
}

// ---------------------------------------------------------------- contents

func blobOf(ctx *Ctx, repo *RepoRecord, oid string) []byte {
	object := ctx.Store.GetObject(repo, oid)
	if object != nil && object.Type == "blob" {
		return object.Body
	}
	return nil
}

func contentJSON(ctx *Ctx, repo *RepoRecord, path, oid string, withContent bool, branch string) map[string]any {
	body := blobOf(ctx, repo, oid)
	if branch == "" {
		branch = repo.DefaultBranch
	}
	parts := strings.Split(path, "/")
	entry := map[string]any{
		"type":         "file",
		"encoding":     "base64",
		"size":         len(body),
		"name":         parts[len(parts)-1],
		"path":         path,
		"sha":          oid,
		"url":          repoURL(ctx, repo) + "/contents/" + path,
		"html_url":     webURL(ctx, repo) + "/blob/" + branch + "/" + path,
		"git_url":      repoURL(ctx, repo) + "/git/blobs/" + oid,
		"download_url": webURL(ctx, repo) + "/raw/" + branch + "/" + path,
	}
	if withContent {
		entry["content"] = base64.StdEncoding.EncodeToString(body)
	}
	return entry
}

func dirJSON(ctx *Ctx, repo *RepoRecord, path, oid, branch string) map[string]any {
	parts := strings.Split(path, "/")
	return map[string]any{
		"type":         "dir",
		"size":         0,
		"name":         parts[len(parts)-1],
		"path":         path,
		"sha":          oid,
		"url":          repoURL(ctx, repo) + "/contents/" + path,
		"html_url":     webURL(ctx, repo) + "/tree/" + branch + "/" + path,
		"git_url":      repoURL(ctx, repo) + "/git/trees/" + oid,
		"download_url": nil,
	}
}

var base64Re = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

func decodeContent(value any) ([]byte, bool) {
	raw, ok := asString(value)
	if !ok {
		return nil, false
	}
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == '\v' || r == '\f' {
			return -1
		}
		return r
	}, raw)
	if !base64Re.MatchString(cleaned) || len(cleaned)%4 != 0 {
		return nil, false
	}
	out, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, false
	}
	return out, true
}

// ---------------------------------------------------------------- pulls: diffs

func linesOf(bytes []byte) []string {
	if len(bytes) == 0 {
		return nil
	}
	lines := strings.Split(string(bytes), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func lineDiff(base, head []string) (int, int) {
	counts := map[string]int{}
	for _, line := range base {
		counts[line]++
	}
	additions := 0
	for _, line := range head {
		if left := counts[line]; left > 0 {
			counts[line] = left - 1
		} else {
			additions++
		}
	}
	deletions := 0
	for _, left := range counts {
		deletions += left
	}
	return additions, deletions
}

func isAncestor(store *Store, repo *RepoRecord, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if next == ancestor {
			return true
		}
		if seen[next] {
			continue
		}
		seen[next] = true
		object := store.GetObject(repo, next)
		if object == nil || object.Type != "commit" {
			continue
		}
		stack = append(stack, parseCommit(object.Body).Parents...)
	}
	return false
}

func mergeBase(store *Store, repo *RepoRecord, ours, theirs string) string {
	oursAncestors := map[string]bool{}
	seen := map[string]bool{}
	stack := []string{ours}
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[next] {
			continue
		}
		seen[next] = true
		oursAncestors[next] = true
		object := store.GetObject(repo, next)
		if object == nil || object.Type != "commit" {
			continue
		}
		stack = append(stack, parseCommit(object.Body).Parents...)
	}
	queue := []string{theirs}
	visited := map[string]bool{}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if visited[next] {
			continue
		}
		visited[next] = true
		if oursAncestors[next] {
			return next
		}
		object := store.GetObject(repo, next)
		if object == nil || object.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(object.Body).Parents...)
	}
	return ""
}

func writeFlatTree(store *Store, repo *RepoRecord, flat map[string]TreeEntry) string {
	var build func(prefix string) []TreeEntry
	build = func(prefix string) []TreeEntry {
		entries := []TreeEntry{}
		dirs := map[string]bool{}
		dirOrder := []string{}
		names := make([]string, 0, len(flat))
		for path := range flat {
			names = append(names, path)
		}
		sort.Strings(names)
		for _, path := range names {
			if prefix != "" && !strings.HasPrefix(path, prefix+"/") {
				continue
			}
			rest := path
			if prefix != "" {
				rest = path[len(prefix)+1:]
			}
			slash := strings.IndexByte(rest, '/')
			if slash < 0 {
				entry := flat[path]
				entries = append(entries, TreeEntry{Mode: entry.Mode, Name: rest, Oid: entry.Oid})
			} else {
				dir := rest[:slash]
				if !dirs[dir] {
					dirs[dir] = true
					dirOrder = append(dirOrder, dir)
				}
			}
		}
		for _, dir := range dirOrder {
			childPrefix := dir
			if prefix != "" {
				childPrefix = prefix + "/" + dir
			}
			entries = append(entries, TreeEntry{Mode: modeDir, Name: dir, Oid: writeTree(store, repo, build(childPrefix))})
		}
		return entries
	}
	return writeTree(store, repo, build(""))
}

func mergeTrees(store *Store, repo *RepoRecord, ancestorTree, oursTree, theirsTree string) (string, []string) {
	ancestor := flattenTree(store, repo, ancestorTree, "")
	ours := flattenTree(store, repo, oursTree, "")
	theirs := flattenTree(store, repo, theirsTree, "")
	pathSet := map[string]bool{}
	var paths []string
	for _, m := range []map[string]TreeEntry{ancestor, ours, theirs} {
		for path := range m {
			if !pathSet[path] {
				pathSet[path] = true
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	merged := map[string]TreeEntry{}
	conflicts := []string{}
	for _, path := range paths {
		baseOid := entryOid(ancestor, path)
		mine := entryOid(ours, path)
		yours := entryOid(theirs, path)
		var picked *TreeEntry
		if mine == yours {
			if e, ok := ours[path]; ok {
				c := e
				picked = &c
			}
		} else if mine == baseOid {
			if e, ok := theirs[path]; ok {
				c := e
				picked = &c
			}
		} else if yours == baseOid {
			if e, ok := ours[path]; ok {
				c := e
				picked = &c
			}
		} else {
			conflicts = append(conflicts, path)
			continue
		}
		if picked != nil {
			merged[path] = *picked
		}
	}
	if len(conflicts) > 0 {
		return "", conflicts
	}
	return writeFlatTree(store, repo, merged), nil
}

func entryOid(m map[string]TreeEntry, path string) string {
	if e, ok := m[path]; ok {
		return e.Oid
	}
	return "\x00missing"
}

// ---------------------------------------------------------------- core surface

func registerCore() {
	route("GET", "/healthz", func(ctx *Ctx) Reply {
		return R(200, map[string]any{"ok": true})
	})

	route("GET", "/api/v3", func(ctx *Ctx) Reply {
		base := ctx.Store.Base
		return RH(200, map[string]any{
			"current_user_url":                     base + "/api/v3/user",
			"current_user_authorizations_html_url": "",
			"authorizations_url":                   base + "/api/v3/authorizations",
			"code_search_url":                      base + "/api/v3/search/code?q={query}{&page,per_page,sort,order}",
			"commit_search_url":                    base + "/api/v3/search/commits?q={query}{&page,per_page,sort,order}",
			"emails_url":                           base + "/api/v3/user/emails",
			"feeds_url":                            base + "/api/v3/feeds",
			"followers_url":                        base + "/api/v3/user/followers",
			"following_url":                        base + "/api/v3/user/following{/target}",
			"gists_url":                            base + "/api/v3/gists{/gist_id}",
			"hub_url":                              "",
			"issue_search_url":                     base + "/api/v3/search/issues?q={query}{&page,per_page,sort,order}",
			"issues_url":                           base + "/api/v3/issues",
			"keys_url":                             base + "/api/v3/user/keys{/key_id}",
			"label_search_url":                     base + "/api/v3/search/labels?q={query}&repository_id={repository_id}{&page,per_page}",
			"notifications_url":                    base + "/api/v3/notifications",
			"organization_url":                     base + "/api/v3/orgs/{org}",
			"organization_repositories_url":        base + "/api/v3/orgs/{org}/repos{?type,page,per_page,sort}",
			"organization_teams_url":               base + "/api/v3/orgs/{org}/teams",
			"public_gists_url":                     base + "/api/v3/gists/public",
			"rate_limit_url":                       base + "/api/v3/rate_limit",
			"repository_url":                       base + "/api/v3/repos/{owner}/{repo}",
			"repository_search_url":                base + "/api/v3/search/repositories?q={query}{&page,per_page,sort,order}",
			"current_user_repositories_url":        base + "/api/v3/user/repos{?type,page,per_page,sort}",
			"starred_url":                          base + "/api/v3/user/starred{/owner}{/repo}",
			"starred_gists_url":                    base + "/api/v3/gists/starred",
			"topic_search_url":                     base + "/api/v3/search/topics?q={query}{&page,per_page}",
			"user_url":                             base + "/api/v3/users/{user}",
			"user_organizations_url":               base + "/api/v3/user/orgs",
			"user_repositories_url":                base + "/api/v3/users/{user}/repos{?type,page,per_page,sort}",
			"user_search_url":                      base + "/api/v3/search/users?q={query}{&page,per_page,sort,order}",
		}, OutHeaders{"x-oauth-scopes": "repo, read:org"})
	})

	route("GET", "/api/v3/meta", func(ctx *Ctx) Reply {
		return R(200, map[string]any{"installed_version": "1.0.0"})
	})

	route("GET", "/api/v3/rate_limit", func(ctx *Ctx) Reply {
		limit := 5000
		return RH(200, map[string]any{
			"resources": map[string]any{
				"core":    map[string]any{"limit": limit, "remaining": limit, "reset": 1798761600, "used": 0},
				"search":  map[string]any{"limit": 30, "remaining": 30, "reset": 1798761600, "used": 0},
				"graphql": map[string]any{"limit": limit, "remaining": limit, "reset": 1798761600, "used": 0},
			},
			"rate": map[string]any{"limit": limit, "remaining": limit, "reset": 1798761600, "used": 0},
		}, OutHeaders{
			"x-ratelimit-limit":     strconv.Itoa(limit),
			"x-ratelimit-remaining": strconv.Itoa(limit),
			"x-ratelimit-used":      "0",
		})
	})

	route("GET", "/api/v3/user", func(ctx *Ctx) Reply {
		if ctx.User == nil {
			return R(401, map[string]any{"message": "Requires authentication"})
		}
		return R(200, userJSON(ctx.User))
	})

	route("GET", "/api/v3/users/:username", func(ctx *Ctx) Reply {
		user := ctx.Store.GetUser(ctx.Params["username"])
		if user == nil {
			return notFoundReply()
		}
		return R(200, userJSON(user))
	})

	route("GET", "/api/v3/user/repos", func(ctx *Ctx) Reply {
		all := ctx.Store.ListRepos(ctx.Login)
		perPage := clampInt(ctx.QueryGet("per_page"), 30, 1, 100)
		page := clampInt(ctx.QueryGet("page"), 1, 1, 10000)
		start := (page - 1) * perPage
		slice := []*RepoRecord{}
		if start < len(all) {
			end := start + perPage
			if end > len(all) {
				end = len(all)
			}
			slice = all[start:end]
		}
		origin := protoOf(ctx.Headers) + "://" + ctx.Host
		base := origin + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage)
		links := []string{}
		lastPage := 1
		if perPage > 0 {
			lastPage = (len(all) + perPage - 1) / perPage
			if lastPage < 1 {
				lastPage = 1
			}
		}
		if start+perPage < len(all) {
			links = append(links, "<"+base+"&page="+strconv.Itoa(page+1)+`>; rel="next"`)
		}
		if page > 1 {
			links = append(links, "<"+base+"&page="+strconv.Itoa(page-1)+`>; rel="prev"`)
			links = append(links, "<"+base+"&page=1>; rel=\"first\"")
		}
		if page < lastPage {
			links = append(links, "<"+base+"&page="+strconv.Itoa(lastPage)+`>; rel="last"`)
		}
		headers := OutHeaders{}
		if len(links) > 0 {
			headers["link"] = strings.Join(links, ", ")
		}
		list := make([]any, 0, len(slice))
		for _, repo := range slice {
			list = append(list, repoJSON(repo))
		}
		return RH(200, list, headers)
	})
}

// ---------------------------------------------------------------- git transport routes

func gitRepoOf(ctx *Ctx) *RepoRecord {
	owner := ctx.Params["owner"]
	name := strings.TrimSuffix(ctx.Params["repo"], ".git")
	if owner == "" || name == "" {
		return nil
	}
	return ctx.Store.GetRepo(owner, name)
}

func noSuchRepo() Reply {
	return RH(404, []byte("Not Found\n"), OutHeaders{"content-type": "text/plain; charset=utf-8"})
}

func registerGit() {
	route("GET", "/:owner/:repo/info/refs", func(ctx *Ctx) Reply {
		asked := ctx.QueryGet("service")
		if asked != "git-upload-pack" && asked != "git-receive-pack" {
			return noSuchRepo()
		}
		repo := gitRepoOf(ctx)
		if repo == nil {
			return noSuchRepo()
		}
		kind := "upload-pack"
		if asked == "git-receive-pack" {
			kind = "receive-pack"
		}
		return RH(200, advertiseRefs(ctx.Store, repo, asked), OutHeaders{
			"content-type":  "application/x-git-" + kind + "-advertisement",
			"cache-control": "no-cache",
		})
	})

	route("POST", "/:owner/:repo/git-upload-pack", func(ctx *Ctx) Reply {
		repo := gitRepoOf(ctx)
		if repo == nil {
			return noSuchRepo()
		}
		body := handleUploadPack(ctx.Store, repo, ctx.Raw)
		return RH(200, body, OutHeaders{"content-type": "application/x-git-upload-pack-result", "cache-control": "no-cache"})
	})

	route("POST", "/:owner/:repo/git-receive-pack", func(ctx *Ctx) Reply {
		repo := gitRepoOf(ctx)
		if repo == nil {
			return noSuchRepo()
		}
		body := handleReceivePack(ctx.Store, repo, ctx.Raw)
		return RH(200, body, OutHeaders{"content-type": "application/x-git-receive-pack-result", "cache-control": "no-cache"})
	})
}

// ---------------------------------------------------------------- REST v3 routes

func registerRest() {
	// -------------------------------------------------------------- repositories

	route("POST", "/api/v3/user/repos", func(ctx *Ctx) Reply {
		body := asObject(ctx)
		name, ok := asString(body["name"])
		if !ok || name == "" || strings.ContainsAny(name, " \t\n/\\") {
			return R(422, map[string]any{"message": "Repository creation failed. name already exists on this account"})
		}
		login := ctx.Login
		if !ctx.HasLogin {
			return R(401, map[string]any{"message": "Requires authentication"})
		}
		var description *string
		if s, ok := asString(body["description"]); ok {
			description = &s
		}
		private, _ := body["private"].(bool)
		created := ctx.Store.CreateRepo(login, name, description, private)
		if created == nil {
			return R(422, map[string]any{"message": "Repository creation failed. name already exists on this account"})
		}
		return RH(201, repoJSON(created), OutHeaders{"location": created.URL})
	})

	route("GET", "/api/v3/repos/:owner/:repo", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		return R(200, repoJSON(repo))
	})

	route("PATCH", "/api/v3/repos/:owner/:repo", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		body := asObject(ctx)
		if _, present := body["description"]; present {
			if s, ok := asString(body["description"]); ok {
				repo.Description = &s
			} else {
				repo.Description = nil
			}
		}
		if v, ok := body["private"].(bool); ok {
			repo.Private = v
			if v {
				repo.Visibility = "private"
			} else {
				repo.Visibility = "public"
			}
		}
		if v, ok := body["archived"].(bool); ok {
			repo.Archived = v
		}
		return R(200, repoJSON(repo))
	})

	route("DELETE", "/api/v3/repos/:owner/:repo", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		ctx.Store.DeleteRepo(repo.FullName)
		return R(204, []byte{})
	})

	// -------------------------------------------------------------- branches and refs

	route("GET", "/api/v3/repos/:owner/:repo/branches", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		list := []any{}
		for _, pair := range ctx.Store.ListRefs(repo, "refs/heads/") {
			list = append(list, map[string]any{
				"name":      strings.TrimPrefix(pair[0], "refs/heads/"),
				"commit":    map[string]any{"sha": pair[1], "url": repoURL(ctx, repo) + "/commits/" + pair[1]},
				"protected": false,
			})
		}
		return R(200, list)
	})

	refJSON := func(ctx *Ctx, repo *RepoRecord, ref, oid string) map[string]any {
		return map[string]any{
			"ref":     ref,
			"node_id": b64(ref),
			"url":     repoURL(ctx, repo) + "/git/" + ref,
			"object":  map[string]any{"sha": oid, "type": "commit", "url": repoURL(ctx, repo) + "/git/commits/" + oid},
		}
	}

	route("GET", "/api/v3/repos/:owner/:repo/git/refs", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		list := []any{}
		for _, pair := range ctx.Store.ListRefs(repo, "") {
			list = append(list, refJSON(ctx, repo, pair[0], pair[1]))
		}
		return R(200, list)
	})

	route("POST", "/api/v3/repos/:owner/:repo/git/refs", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		body := asObject(ctx)
		raw, _ := asString(body["ref"])
		sha, _ := asString(body["sha"])
		if !strings.HasPrefix(raw, "refs/") || sha == "" {
			return R(422, map[string]any{"message": "ref and sha are required"})
		}
		if !ctx.Store.HasObject(repo, sha) {
			return R(422, map[string]any{"message": "Object does not exist"})
		}
		if ctx.Store.HasRef(repo, raw) {
			return R(422, map[string]any{"message": "Reference already exists"})
		}
		ctx.Store.SetRef(repo, raw, sha)
		base := ctx.Store.Base
		return R(201, map[string]any{
			"ref":     raw,
			"node_id": b64(raw),
			"url":     base + "/api/v3/repos/" + repo.FullName + "/git/refs/" + strings.TrimPrefix(raw, "refs/"),
			"object":  map[string]any{"sha": sha, "type": "commit", "url": base + "/api/v3/repos/" + repo.FullName + "/git/commits/" + sha},
		})
	})

	route("PATCH", "/api/v3/repos/:owner/:repo/git/refs/*", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		raw := ctx.Params["path"]
		sha, _ := asString(asObject(ctx)["sha"])
		if !ctx.Store.HasRef(repo, raw) {
			return R(422, map[string]any{"message": "Reference does not exist"})
		}
		if sha == "" {
			return R(422, map[string]any{"message": "sha is required"})
		}
		ctx.Store.SetRef(repo, raw, sha)
		return R(200, map[string]any{
			"ref":    raw,
			"object": map[string]any{"sha": sha, "type": "commit", "url": ctx.Store.Base + "/api/v3/repos/" + repo.FullName + "/git/commits/" + sha},
		})
	})

	route("DELETE", "/api/v3/repos/:owner/:repo/git/refs/*", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		raw := ctx.Params["path"]
		if !ctx.Store.HasRef(repo, raw) {
			return R(422, map[string]any{"message": "Reference does not exist"})
		}
		ctx.Store.DeleteRef(repo, raw)
		return R(204, []byte{})
	})

	route("GET", "/api/v3/repos/:owner/:repo/git/refs/heads", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		list := []any{}
		for _, pair := range ctx.Store.ListRefs(repo, "refs/heads/") {
			list = append(list, refJSON(ctx, repo, pair[0], pair[1]))
		}
		return R(200, list)
	})

	// -------------------------------------------------------------- commits

	route("GET", "/api/v3/repos/:owner/:repo/commits", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		branch := ctx.QueryGet("sha")
		if branch == "" {
			branch = ctx.QueryGet("branch")
		}
		tip := branchTip(ctx.Store, repo, branch)
		if tip == "" {
			return R(200, []any{})
		}
		list := []any{}
		for _, oid := range walkCommits(ctx.Store, repo, tip, 30) {
			list = append(list, commitJSON(ctx, repo, oid))
		}
		return R(200, list)
	})

	// -------------------------------------------------------------- contents

	route("GET", "/api/v3/repos/:owner/:repo/contents/*", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		store := ctx.Store
		path := strings.Trim(ctx.Params["path"], "/")
		branch := ctx.QueryGet("ref")
		if branch == "" {
			branch = repo.DefaultBranch
		}
		branchCommit, ok := commitAt(store, repo, branchTip(store, repo, branch))
		if !ok {
			return notFoundReply()
		}
		rootTree := branchCommit.Tree
		if path == "" {
			list := []any{}
			for _, entry := range readTree(store, repo, rootTree) {
				if entry.Mode == modeDir {
					list = append(list, dirJSON(ctx, repo, entry.Name, entry.Oid, branch))
				} else {
					list = append(list, contentJSON(ctx, repo, entry.Name, entry.Oid, false, branch))
				}
			}
			return R(200, list)
		}
		entry, found := fileAt(store, repo, rootTree, path)
		if !found {
			return notFoundReply()
		}
		if entry.Mode == modeDir {
			list := []any{}
			for _, child := range readTree(store, repo, entry.Oid) {
				childPath := path + "/" + child.Name
				if child.Mode == modeDir {
					list = append(list, dirJSON(ctx, repo, childPath, child.Oid, branch))
				} else {
					list = append(list, contentJSON(ctx, repo, childPath, child.Oid, false, branch))
				}
			}
			return R(200, list)
		}
		return R(200, contentJSON(ctx, repo, path, entry.Oid, true, branch))
	})

	route("PUT", "/api/v3/repos/:owner/:repo/contents/*", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		store := ctx.Store
		path := strings.Trim(ctx.Params["path"], "/")
		if path == "" {
			return R(422, map[string]any{"message": "Invalid path"})
		}
		body := asObject(ctx)
		bytes, ok := decodeContent(body["content"])
		message, hasMessage := asString(body["message"])
		if !ok {
			return R(422, map[string]any{"message": "content is not valid base64"})
		}
		if !hasMessage || strings.TrimSpace(message) == "" {
			return R(422, map[string]any{"message": "message is required"})
		}
		branch, hasBranch := asString(body["branch"])
		if !hasBranch || branch == "" {
			branch = repo.DefaultBranch
		}
		ownTip := store.GetRef(repo, "refs/heads/"+branch)
		tip := ownTip
		if tip == "" && branch != repo.DefaultBranch {
			tip = store.GetRef(repo, "refs/heads/"+repo.DefaultBranch)
		}
		tipCommit, hasTip := commitAt(store, repo, tip)
		var existing TreeEntry
		hasExisting := false
		if hasTip {
			existing, hasExisting = fileAt(store, repo, tipCommit.Tree, path)
		}
		if expected, hasExpected := asString(body["sha"]); hasExpected {
			if !hasExisting || existing.Oid != expected {
				return R(409, map[string]any{"message": "sha does not match"})
			}
		}
		if _, hasExpected := asString(body["sha"]); !hasExpected && ownTip != "" && hasExisting {
			return R(409, map[string]any{"message": "sha does not match"})
		}
		blobOid := store.PutObject(repo, "blob", bytes)
		treeOid := upsertPath(store, repo, tipCommit.Tree, splitNonEmpty(path, "/"), blobOid, false)
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		commitOid := writeCommit(store, repo, treeOid, parents, authorFor(ctx), message)
		store.SetRef(repo, "refs/heads/"+branch, commitOid)
		repo.PushedAt = EPOCH
		return RH(201, map[string]any{
			"content": contentJSON(ctx, repo, path, blobOid, true, branch),
			"commit": map[string]any{
				"sha":     commitOid,
				"message": message,
				"node_id": b64(commitOid),
				"url":     repoURL(ctx, repo) + "/git/commits/" + commitOid,
			},
		}, OutHeaders{"location": repoURL(ctx, repo) + "/contents/" + path})
	})

	route("DELETE", "/api/v3/repos/:owner/:repo/contents/*", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		store := ctx.Store
		path := strings.Trim(ctx.Params["path"], "/")
		body := asObject(ctx)
		branch, hasBranch := asString(body["branch"])
		if !hasBranch || branch == "" {
			branch = repo.DefaultBranch
		}
		tip := store.GetRef(repo, "refs/heads/"+branch)
		tipCommit, hasTip := commitAt(store, repo, tip)
		var existing TreeEntry
		hasExisting := false
		if hasTip {
			existing, hasExisting = fileAt(store, repo, tipCommit.Tree, path)
		}
		if !hasExisting {
			return notFoundReply()
		}
		if expected, hasExpected := asString(body["sha"]); hasExpected && expected != existing.Oid {
			return R(409, map[string]any{"message": "sha does not match"})
		}
		treeOid := upsertPath(store, repo, tipCommit.Tree, splitNonEmpty(path, "/"), "", true)
		message, hasMessage := asString(body["message"])
		if !hasMessage || message == "" {
			message = "Delete " + path
		}
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		commitOid := writeCommit(store, repo, treeOid, parents, authorFor(ctx), message)
		store.SetRef(repo, "refs/heads/"+branch, commitOid)
		repo.PushedAt = EPOCH
		return R(200, map[string]any{
			"content": nil,
			"commit": map[string]any{
				"sha":     commitOid,
				"message": message,
				"node_id": b64(commitOid),
				"url":     repoURL(ctx, repo) + "/git/commits/" + commitOid,
			},
		})
	})

	// -------------------------------------------------------------- labels

	route("GET", "/api/v3/repos/:owner/:repo/labels", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		list := []any{}
		for _, label := range ctx.Store.ListLabels(repo) {
			list = append(list, labelJSON(label))
		}
		return R(200, list)
	})

	route("POST", "/api/v3/repos/:owner/:repo/labels", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		body := asObject(ctx)
		name, ok := asString(body["name"])
		if !ok || strings.TrimSpace(name) == "" {
			return R(422, map[string]any{"message": "Validation Failed"})
		}
		if ctx.Store.GetLabel(repo, name) != nil {
			return R(422, map[string]any{"message": "Validation Failed"})
		}
		color := "ededed"
		if c, ok := asString(body["color"]); ok && c != "" {
			color = strings.TrimPrefix(c, "#")
		}
		var description *string
		if d, ok := asString(body["description"]); ok {
			description = &d
		}
		label := ctx.Store.CreateLabel(repo, name, color, description)
		return RH(201, labelJSON(label), OutHeaders{"location": label.URL})
	})

	// -------------------------------------------------------------- issues

	route("POST", "/api/v3/repos/:owner/:repo/issues", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		body := asObject(ctx)
		title, ok := asString(body["title"])
		if !ok {
			return R(422, map[string]any{"message": "Validation Failed"})
		}
		labels := []string{}
		if raw, ok := body["labels"].([]any); ok {
			for _, item := range raw {
				if s, ok := item.(string); ok {
					labels = append(labels, s)
				}
			}
		}
		if len(labels) > 0 {
			known := map[string]bool{}
			for _, label := range ctx.Store.ListLabels(repo) {
				known[label.Name] = true
			}
			unknown := []any{}
			for _, name := range labels {
				if !known[name] {
					unknown = append(unknown, map[string]any{"resource": "Issue", "code": "invalid", "field": "labels", "value": name})
				}
			}
			if len(unknown) > 0 {
				return R(422, map[string]any{"message": "Validation Failed", "errors": unknown})
			}
		}
		issueBody, _ := asString(body["body"])
		user := ctx.Login
		if user == "" {
			user = repo.OwnerLogin
		}
		issue := ctx.Store.CreateIssue(repo, title, issueBody, user, "open", labels, true)
		return RH(201, issueJSON(issue), OutHeaders{"location": issue.URL})
	})

	route("GET", "/api/v3/repos/:owner/:repo/issues", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		state := ctx.QueryGet("state")
		if state == "" {
			state = "open"
		}
		if state != "open" && state != "closed" && state != "all" {
			return R(422, map[string]any{"message": "Validation Failed"})
		}
		labels := []string{}
		for _, value := range ctx.QueryAll("labels") {
			for _, part := range strings.Split(value, ",") {
				if part != "" {
					labels = append(labels, part)
				}
			}
		}
		list := []any{}
		for _, issue := range ctx.Store.ListIssues(repo, state, labels) {
			list = append(list, issueJSON(issue))
		}
		if ctx.QueryGet("direction") != "asc" {
			for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
				list[i], list[j] = list[j], list[i]
			}
		}
		return R(200, list)
	})

	route("GET", "/api/v3/repos/:owner/:repo/issues/:number", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		number, ok := numberOf(ctx, "number")
		if !ok {
			return notFoundReply()
		}
		issue := ctx.Store.GetIssue(repo, number)
		if issue == nil {
			return notFoundReply()
		}
		return R(200, issueJSON(issue))
	})

	route("PATCH", "/api/v3/repos/:owner/:repo/issues/:number", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		number, ok := numberOf(ctx, "number")
		if !ok || ctx.Store.GetIssue(repo, number) == nil {
			return notFoundReply()
		}
		body := asObject(ctx)
		patch := UpdateIssuePatch{}
		if title, ok := asString(body["title"]); ok {
			patch.Title = &title
		}
		if _, present := body["body"]; present {
			value, _ := asString(body["body"])
			patch.Body = &value
		}
		if state, ok := asString(body["state"]); ok && (state == "open" || state == "closed") {
			patch.State = &state
		}
		issue := ctx.Store.UpdateIssue(repo, number, patch)
		if issue == nil {
			return notFoundReply()
		}
		return R(200, issueJSON(issue))
	})

	// -------------------------------------------------------------- comments

	route("POST", "/api/v3/repos/:owner/:repo/issues/:number/comments", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		number, ok := numberOf(ctx, "number")
		if !ok {
			return notFoundReply()
		}
		body := asObject(ctx)
		text, ok := asString(body["body"])
		if !ok {
			return R(422, map[string]any{"message": "Validation Failed"})
		}
		user := ctx.Login
		if user == "" {
			user = repo.OwnerLogin
		}
		comment := ctx.Store.CreateComment(repo, number, text, user)
		if comment == nil {
			return notFoundReply()
		}
		return RH(201, commentJSON(comment), OutHeaders{"location": comment.URL})
	})

	route("GET", "/api/v3/repos/:owner/:repo/issues/:number/comments", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		number, ok := numberOf(ctx, "number")
		if !ok {
			return notFoundReply()
		}
		list := []any{}
		for _, comment := range ctx.Store.ListComments(repo, number, true) {
			list = append(list, commentJSON(comment))
		}
		return R(200, list)
	})

	// -------------------------------------------------------------- pulls

	route("POST", "/api/v3/repos/:owner/:repo/pulls", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		store := ctx.Store
		body := asObject(ctx)
		title, ok := asString(body["title"])
		if !ok {
			return R(422, map[string]any{"message": "Validation Failed"})
		}
		headRaw, _ := asString(body["head"])
		if headRaw == "" {
			headRaw = repo.DefaultBranch
		}
		headRef := headRaw
		if i := strings.IndexByte(headRaw, ':'); i >= 0 {
			headRef = headRaw[i+1:]
		}
		headSha, hasHead := store.ResolveRef(repo, headRef)
		if !hasHead {
			headSha = branchTip(store, repo, repo.DefaultBranch)
		}
		baseRef, _ := asString(body["base"])
		if baseRef == "" {
			baseRef = repo.DefaultBranch
		}
		baseSha, _ := store.ResolveRef(repo, baseRef)
		pullBody, _ := asString(body["body"])
		user := ctx.Login
		if user == "" {
			user = repo.OwnerLogin
		}
		pull := store.CreatePull(repo, title, pullBody, user, headRef, headSha, baseRef, baseSha)
		pull.Base.Ref = baseRef
		pull.Base.Label = repo.OwnerLogin + ":" + baseRef
		return RH(201, pullJSON(pull), OutHeaders{"location": pull.URL})
	})

	route("GET", "/api/v3/repos/:owner/:repo/pulls", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		state := ctx.QueryGet("state")
		if state == "" {
			state = "open"
		}
		if state != "open" && state != "closed" && state != "all" {
			return R(422, map[string]any{"message": "Validation Failed"})
		}
		list := []any{}
		for _, pull := range ctx.Store.ListPulls(repo, state) {
			list = append(list, pullJSON(pull))
		}
		return R(200, list)
	})

	route("GET", "/api/v3/repos/:owner/:repo/pulls/:number", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		number, ok := numberOf(ctx, "number")
		if !ok {
			return notFoundReply()
		}
		pull := ctx.Store.GetPull(repo, number)
		if pull == nil {
			return notFoundReply()
		}
		return R(200, pullJSON(pull))
	})

	route("GET", "/api/v3/repos/:owner/:repo/pulls/:number/files", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		store := ctx.Store
		number, ok := numberOf(ctx, "number")
		if !ok {
			return notFoundReply()
		}
		pull := store.GetPull(repo, number)
		if pull == nil {
			return notFoundReply()
		}
		baseCommit, _ := commitAt(store, repo, pull.Base.Sha)
		headCommit, _ := commitAt(store, repo, pull.Head.Sha)
		before := flattenTree(store, repo, baseCommit.Tree, "")
		after := flattenTree(store, repo, headCommit.Tree, "")
		nameSet := map[string]bool{}
		var names []string
		for _, m := range []map[string]TreeEntry{before, after} {
			for name := range m {
				if !nameSet[name] {
					nameSet[name] = true
					names = append(names, name)
				}
			}
		}
		sort.Strings(names)
		files := []any{}
		for _, name := range names {
			oldEntry, hasOld := before[name]
			newEntry, hasNew := after[name]
			if hasOld && hasNew && oldEntry.Oid == newEntry.Oid {
				continue
			}
			status := "modified"
			if !hasOld {
				status = "added"
			} else if !hasNew {
				status = "removed"
			}
			var oldLines, newLines []string
			if hasOld {
				oldLines = linesOf(blobOf(ctx, repo, oldEntry.Oid))
			}
			if hasNew {
				newLines = linesOf(blobOf(ctx, repo, newEntry.Oid))
			}
			additions, deletions := lineDiff(oldLines, newLines)
			sha := ""
			if hasNew {
				sha = newEntry.Oid
			} else if hasOld {
				sha = oldEntry.Oid
			}
			files = append(files, map[string]any{
				"filename":     name,
				"status":       status,
				"additions":    additions,
				"deletions":    deletions,
				"changes":      additions + deletions,
				"blob_url":     repoURL(ctx, repo) + "/git/blobs/" + sha,
				"raw_url":      webURL(ctx, repo) + "/raw/" + pull.Head.Sha + "/" + name,
				"contents_url": repoURL(ctx, repo) + "/contents/" + name,
				"sha":          sha,
			})
		}
		return R(200, files)
	})

	route("PUT", "/api/v3/repos/:owner/:repo/pulls/:number/merge", func(ctx *Ctx) Reply {
		repo := repoOf(ctx)
		if repo == nil {
			return notFoundReply()
		}
		store := ctx.Store
		number, ok := numberOf(ctx, "number")
		if !ok {
			return notFoundReply()
		}
		pull := store.GetPull(repo, number)
		if pull == nil {
			return notFoundReply()
		}
		if pull.Merged || pull.State == "merged" {
			return R(405, map[string]any{"message": "Pull Request is not mergeable"})
		}
		headSha := pull.Head.Sha
		baseTip := branchTip(store, repo, pull.Base.Ref)
		headCommit, hasHead := commitAt(store, repo, headSha)
		message := "Merge pull request #" + strconv.Itoa(pull.Number) + " from " + pull.Head.Ref

		finish := func(sha string) Reply {
			store.SetRef(repo, "refs/heads/"+pull.Base.Ref, sha)
			store.MergePull(repo, pull.Number, sha)
			repo.PushedAt = EPOCH
			return R(200, map[string]any{"merged": true, "sha": sha, "message": "Pull Request successfully merged"})
		}

		if !hasHead {
			baseCommit, ok := commitAt(store, repo, baseTip)
			if !ok {
				return R(405, map[string]any{"message": "Pull Request is not mergeable"})
			}
			var parents []string
			if baseTip != "" {
				parents = []string{baseTip}
			}
			return finish(writeCommit(store, repo, baseCommit.Tree, parents, authorFor(ctx), message))
		}

		if baseTip == "" || isAncestor(store, repo, baseTip, headSha) {
			store.MergePull(repo, pull.Number, headSha)
			store.SetRef(repo, "refs/heads/"+pull.Base.Ref, headSha)
			repo.PushedAt = EPOCH
			return R(200, map[string]any{"merged": true, "sha": headSha, "message": "Pull Request successfully merged"})
		}

		baseCommit, _ := commitAt(store, repo, baseTip)
		ancestor := mergeBase(store, repo, baseTip, headSha)
		ancestorTree := ""
		if ancestor != "" {
			if c, ok := commitAt(store, repo, ancestor); ok {
				ancestorTree = c.Tree
			}
		}
		tree, conflicts := mergeTrees(store, repo, ancestorTree, baseCommit.Tree, headCommit.Tree)
		if tree == "" {
			return R(405, map[string]any{"message": "Pull Request is not mergeable: " + strings.Join(conflicts, ", ")})
		}
		return finish(writeCommit(store, repo, tree, []string{baseTip, headSha}, authorFor(ctx), message))
	})
}
