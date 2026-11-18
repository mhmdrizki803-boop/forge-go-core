package main

// Stateless MCP JSON-RPC endpoint and tool catalogue. Tool calls use the same store
// as REST and GraphQL, keeping every public view immediately consistent.

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

const rpcProtocolVersion = "2024-11-05"
const rpcServerName = "open-forge"
const rpcServerVersion = "1.0.0"

type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

func schemaObj(properties map[string]any, required []string) map[string]any {
	props := map[string]any{
		"owner": map[string]any{"type": "string", "description": "Repository owner login."},
		"repo":  map[string]any{"type": "string", "description": "Repository name."},
	}
	for key, value := range properties {
		props[key] = value
	}
	req := []any{"owner", "repo"}
	for _, item := range required {
		req = append(req, item)
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             req,
		"additionalProperties": false,
	}
}

var mcpTools = []ToolDef{
	{
		Name:        "create_repo",
		Description: "Create a repository for the caller and return it.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "Repository name."},
				"description": map[string]any{"type": "string", "description": "One line about the repository."},
				"private":     map[string]any{"type": "boolean", "description": "True for a private repository."},
			},
			"required": []any{"name"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Open an issue on a repository and return the created issue.",
		InputSchema: schemaObj(map[string]any{
			"title":  map[string]any{"type": "string", "description": "Issue title."},
			"body":   map[string]any{"type": "string", "description": "Issue body in Markdown."},
			"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Label names to attach."},
		}, []string{"title"}),
	},
	{
		Name:        "list_issues",
		Description: "List a repository's issues, newest first, filtered by state.",
		InputSchema: schemaObj(map[string]any{
			"state":  map[string]any{"type": "string", "enum": []any{"open", "closed", "all"}, "description": "Issue state filter."},
			"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only issues carrying every one of these labels."},
			"limit":  map[string]any{"type": "integer", "description": "Maximum issues to return."},
		}, []string{}),
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: schemaObj(map[string]any{
			"title": map[string]any{"type": "string", "description": "Pull request title."},
			"body":  map[string]any{"type": "string", "description": "Pull request body in Markdown."},
			"head":  map[string]any{"type": "string", "description": "Head branch name (defaults to the default branch)."},
			"base":  map[string]any{"type": "string", "description": "Base branch name (defaults to the default branch)."},
		}, []string{"title"}),
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge an open pull request into its base branch.",
		InputSchema: schemaObj(map[string]any{
			"pull_number":  map[string]any{"type": "integer", "description": "The pull request number."},
			"commit_title": map[string]any{"type": "string", "description": "Title for the merge commit."},
			"merge_method": map[string]any{"type": "string", "enum": []any{"merge", "squash", "rebase"}, "description": "Merge strategy."},
		}, []string{"pull_number"}),
	},
	{
		Name:        "get_repo",
		Description: "Read one repository: its name, owner, description, visibility and branches.",
		InputSchema: schemaObj(map[string]any{}, []string{}),
	},
	{
		Name:        "list_branches",
		Description: "List the branches of a repository with the commit each one points at.",
		InputSchema: schemaObj(map[string]any{
			"limit": map[string]any{"type": "integer", "description": "Maximum branches to return."},
		}, []string{}),
	},
}

func mcpToolsListResult() map[string]any {
	tools := make([]any, 0, len(mcpTools))
	for _, tool := range mcpTools {
		tools = append(tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": tool.InputSchema,
		})
	}
	return map[string]any{"tools": tools}
}

// ---------------------------------------------------------------- helpers

type rpcErrorValue struct {
	Code    int
	Message string
}

func (e *rpcErrorValue) Error() string { return e.Message }

func badArgs(message string) error { return &rpcErrorValue{Code: -32602, Message: message} }

var integerText = regexp.MustCompile(`^\d+$`)

func asNumber(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case int:
		return v, true
	case string:
		if integerText.MatchString(v) {
			n, err := jsonNumber(v)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func jsonNumber(text string) (int, error) {
	n := 0
	for i := 0; i < len(text); i++ {
		n = n*10 + int(text[i]-'0')
	}
	return n, nil
}

// findToolName resolves a tool name exactly, then by its suffix after a server prefix.
func findToolName(name string) (string, bool) {
	for _, tool := range mcpTools {
		if tool.Name == name {
			return tool.Name, true
		}
	}
	suffix := name
	if i := strings.LastIndex(name, "__"); i >= 0 {
		suffix = name[i+2:]
	}
	for _, tool := range mcpTools {
		if strings.EqualFold(tool.Name, suffix) {
			return tool.Name, true
		}
	}
	return "", false
}

func repoFor(store *Store, args map[string]any) (*RepoRecord, error) {
	owner := asStr(args["owner"])
	repo := asStr(args["repo"])
	if repo == "" {
		repo = asStr(args["name"])
	}
	if owner == "" {
		return nil, badArgs("Missing required argument: owner")
	}
	if repo == "" {
		return nil, badArgs("Missing required argument: repo")
	}
	found := store.GetRepo(owner, repo)
	if found == nil {
		return nil, badArgs("No such repository: " + owner + "/" + repo)
	}
	return found, nil
}

func branchTipMCP(store *Store, repo *RepoRecord, branch string) string {
	oid, _ := store.GetRefOk(repo, "refs/heads/"+branch)
	return oid
}

// ---------------------------------------------------------------- tool handlers

func runTool(store *Store, login, name string, args map[string]any) (any, error) {
	switch name {
	case "create_repo":
		repoName := asStr(args["name"])
		if strings.TrimSpace(repoName) == "" {
			return nil, badArgs("Missing required argument: name")
		}
		ownerLogin := login
		if ownerLogin == "" {
			ownerLogin = "viewer"
		}
		description := asStr(args["description"])
		private, _ := args["private"].(bool)
		made := store.CreateRepo(ownerLogin, repoName, &description, private)
		if made == nil {
			return nil, badArgs("Repository creation failed: " + ownerLogin + "/" + repoName + " already exists")
		}
		return map[string]any{
			"type":           "Repository",
			"name":           made.Name,
			"full_name":      made.FullName,
			"owner":          ownerLogin,
			"private":        made.Private,
			"default_branch": made.DefaultBranch,
			"description":    sp(made.Description),
		}, nil

	case "create_issue":
		repo, err := repoFor(store, args)
		if err != nil {
			return nil, err
		}
		title, ok := asString(args["title"])
		if !ok {
			return nil, badArgs("Missing required argument: title")
		}
		labels := []string{}
		if list, ok := args["labels"].([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok {
					labels = append(labels, s)
				}
			}
		}
		user := login
		if user == "" {
			user = repo.OwnerLogin
		}
		issue := store.CreateIssue(repo, title, asStr(args["body"]), user, "open", labels, true)
		return issueJSON(issue), nil

	case "list_issues":
		repo, err := repoFor(store, args)
		if err != nil {
			return nil, err
		}
		state := asStr(args["state"])
		if state == "" {
			state = "open"
		}
		if state != "open" && state != "closed" && state != "all" {
			return nil, badArgs("Invalid state: " + state)
		}
		labels := []string{}
		if list, ok := args["labels"].([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok {
					labels = append(labels, s)
				}
			}
		}
		list := []any{}
		for _, issue := range store.ListIssues(repo, state, labels) {
			list = append(list, issueJSON(issue))
		}
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
		if limit, ok := asNumber(args["limit"]); ok && limit < len(list) {
			list = list[:limit]
		}
		return list, nil

	case "create_pull_request":
		repo, err := repoFor(store, args)
		if err != nil {
			return nil, err
		}
		title, ok := asString(args["title"])
		if !ok {
			return nil, badArgs("Missing required argument: title")
		}
		headRaw := asStr(args["head"])
		if headRaw == "" {
			headRaw = repo.DefaultBranch
		}
		headRef := headRaw
		if i := strings.IndexByte(headRaw, ':'); i >= 0 {
			headRef = headRaw[i+1:]
		}
		headSha := branchTipMCP(store, repo, headRef)
		baseRef := asStr(args["base"])
		if baseRef == "" {
			baseRef = repo.DefaultBranch
		}
		baseSha := branchTipMCP(store, repo, baseRef)
		user := login
		if user == "" {
			user = repo.OwnerLogin
		}
		pull := store.CreatePull(repo, title, asStr(args["body"]), user, headRef, headSha, baseRef, baseSha)
		return pullJSON(pull), nil

	case "merge_pull_request":
		repo, err := repoFor(store, args)
		if err != nil {
			return nil, err
		}
		number, ok := asNumber(args["pull_number"])
		if !ok {
			number, ok = asNumber(args["number"])
		}
		if !ok {
			return nil, badArgs("Missing required argument: pull_number")
		}
		pull := store.GetPull(repo, number)
		if pull == nil {
			return nil, badArgs("No such pull request: #" + strconv.Itoa(number))
		}
		if pull.Merged {
			return map[string]any{"merged": true, "sha": sp(pull.MergeCommitSha), "message": "Pull Request already merged"}, nil
		}
		headSha := pull.Head.Sha
		if headSha != "" && store.HasObject(repo, headSha) {
			store.SetRef(repo, "refs/heads/"+pull.Base.Ref, headSha)
		}
		store.MergePull(repo, pull.Number, headSha)
		return map[string]any{"merged": true, "sha": headSha, "message": "Pull Request successfully merged"}, nil

	case "get_repo", "get_repository":
		repo, err := repoFor(store, args)
		if err != nil {
			return nil, err
		}
		branches := []any{}
		for _, pair := range store.ListRefs(repo, "refs/heads/") {
			branches = append(branches, map[string]any{
				"name":   strings.TrimPrefix(pair[0], "refs/heads/"),
				"commit": map[string]any{"sha": pair[1]},
			})
		}
		return map[string]any{
			"id":             repo.ID,
			"name":           repo.Name,
			"full_name":      repo.FullName,
			"owner":          map[string]any{"login": repo.OwnerLogin},
			"description":    sp(repo.Description),
			"private":        repo.Private,
			"default_branch": repo.DefaultBranch,
			"url":            repo.URL,
			"html_url":       repo.HTMLURL,
			"created_at":     repo.CreatedAt,
			"branches":       branches,
		}, nil

	case "list_branches":
		repo, err := repoFor(store, args)
		if err != nil {
			return nil, err
		}
		branches := []any{}
		for _, pair := range store.ListRefs(repo, "refs/heads/") {
			branches = append(branches, map[string]any{
				"name":      strings.TrimPrefix(pair[0], "refs/heads/"),
				"commit":    map[string]any{"sha": pair[1], "url": repo.URL + "/commits/" + pair[1]},
				"protected": false,
			})
		}
		if limit, ok := asNumber(args["limit"]); ok && limit < len(branches) {
			branches = branches[:limit]
		}
		return branches, nil
	}
	return nil, &rpcErrorValue{Code: -32601, Message: "Unknown tool: " + name}
}

// ---------------------------------------------------------------- dispatch

type dispatchResult struct {
	status  int
	body    any
	hasBody bool
}

func rpcErrorBody(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}

func rpcResultBody(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func dispatchRPC(store *Store, login string, hasLogin bool, request map[string]any) dispatchResult {
	id := any(nil)
	if raw, ok := request["id"]; ok {
		id = raw
	}
	method, _ := request["method"].(string)
	if method == "" {
		return dispatchResult{status: 200, body: rpcErrorBody(id, -32600, "Invalid Request"), hasBody: true}
	}

	switch method {
	case "initialize":
		return dispatchResult{status: 200, hasBody: true, body: rpcResultBody(id, map[string]any{
			"protocolVersion": rpcProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": rpcServerName, "version": rpcServerVersion},
		})}

	case "notifications/initialized", "ping", "notifications/cancelled":
		if _, hasID := request["id"]; !hasID {
			return dispatchResult{status: 202}
		}
		return dispatchResult{status: 200, hasBody: true, body: rpcResultBody(id, map[string]any{})}

	case "tools/list":
		return dispatchResult{status: 200, hasBody: true, body: rpcResultBody(id, mcpToolsListResult())}

	case "tools/call":
		if !hasLogin || login == "" {
			return dispatchResult{status: 401, hasBody: true, body: map[string]any{"message": "Requires authentication"}}
		}
		params := asMap(request["params"])
		name := ""
		args := map[string]any{}
		if params != nil {
			name = asStr(params["name"])
			if m := asMap(params["arguments"]); m != nil {
				args = m
			}
		}
		canonical, known := findToolName(name)
		if !known {
			return dispatchResult{status: 200, hasBody: true, body: rpcErrorBody(id, -32602, "Unknown tool")}
		}
		result, err := runTool(store, login, canonical, args)
		if err != nil {
			code := -32602
			if rpcErr, ok := err.(*rpcErrorValue); ok {
				code = rpcErr.Code
			}
			return dispatchResult{status: 200, hasBody: true, body: rpcErrorBody(id, code, err.Error())}
		}
		payload, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			payload = []byte("null")
		}
		return dispatchResult{status: 200, hasBody: true, body: rpcResultBody(id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(payload)}},
			"isError": false,
		})}
	}

	return dispatchResult{status: 200, hasBody: true, body: rpcErrorBody(id, -32601, "Method not found: "+method)}
}

// handleMcp answers POST /mcp: one request or a batch.
func handleMcp(ctx *Ctx) Reply {
	store := ctx.Store
	text := strings.TrimSpace(string(ctx.Raw))
	if text == "" {
		return R(200, rpcErrorBody(nil, -32600, "Invalid Request"))
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return R(200, rpcErrorBody(nil, -32700, "Parse error"))
	}
	if list, ok := parsed.([]any); ok {
		if len(list) == 0 {
			return R(200, rpcErrorBody(nil, -32600, "Invalid Request"))
		}
		answers := []any{}
		status := 200
		for _, item := range list {
			request, _ := item.(map[string]any)
			if request == nil {
				answers = append(answers, rpcErrorBody(nil, -32600, "Invalid Request"))
				continue
			}
			answer := dispatchRPC(store, ctx.Login, ctx.HasLogin, request)
			if answer.hasBody {
				answers = append(answers, answer.body)
			}
			if answer.status == 401 {
				status = 401
			}
		}
		if len(answers) == 0 {
			return R(202, []byte{})
		}
		return R(status, answers)
	}
	request, ok := parsed.(map[string]any)
	if !ok {
		return R(200, rpcErrorBody(nil, -32600, "Invalid Request"))
	}
	answer := dispatchRPC(store, ctx.Login, ctx.HasLogin, request)
	if !answer.hasBody {
		return R(answer.status, []byte{})
	}
	return R(answer.status, answer.body)
}

func registerMcp() {
	route("POST", "/mcp", func(ctx *Ctx) Reply {
		return handleMcp(ctx)
	})
}
