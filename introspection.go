package main

// The GraphQL schema metadata: the type table introspection describes.

import (
	"strings"
)

type IntroArg struct {
	Name         string
	Description  string
	DefaultValue any
	Type         *IntroType
}

type IntroFieldRef struct {
	Name              string
	Description       string
	Args              []*IntroArg
	Type              *IntroType
	IsDeprecated      bool
	DeprecationReason *string
}

type IntroType struct {
	Name          *string
	Kind          string
	Description   *string
	Fields        []*IntroFieldRef
	Interfaces    []*IntroType
	EnumValues    []map[string]any
	PossibleTypes []*IntroType
	OfType        *IntroType
}

var typeIndex = map[string]*IntroType{}
var introOrder []*IntroType

var scalarNames = []string{"String", "Int", "Float", "Boolean", "ID", "URI", "DateTime"}
var nodeTypes = []string{"Repository", "Issue", "PullRequest", "User", "Label", "IssueComment"}
var gitTypes = []string{"Commit", "Tree", "Blob"}

type objectSource struct {
	Name       string
	Fields     [][2]string
	Interfaces []string
}

type enumSource struct {
	Name   string
	Values []string
}

type interfaceSource struct {
	Name          string
	Fields        [][2]string
	PossibleTypes []string
}

var objectSources = []objectSource{
	{Name: "Query", Fields: [][2]string{
		{"viewer", "User"},
		{"repository", "Repository"},
		{"node", "Node"},
		{"search", "SearchResultItemConnection"},
		{"__schema", "__Schema!"},
		{"__type", "__Type"},
	}},
	{Name: "Mutation", Fields: [][2]string{
		{"createIssue", "CreateIssuePayload"},
		{"closeIssue", "CloseIssuePayload"},
		{"reopenIssue", "ReopenIssuePayload"},
		{"addComment", "AddCommentPayload"},
		{"createPullRequest", "CreatePullRequestPayload"},
		{"mergePullRequest", "MergePullRequestPayload"},
	}},
	{Name: "Repository", Interfaces: []string{"Node"}, Fields: [][2]string{
		{"id", "ID!"},
		{"databaseId", "Int"},
		{"owner", "User!"},
		{"viewerPermission", "RepositoryPermission"},
		{"hasIssuesEnabled", "Boolean!"},
		{"hasWikiEnabled", "Boolean!"},
		{"isEmpty", "Boolean!"},
		{"name", "String!"},
		{"nameWithOwner", "String!"},
		{"description", "String"},
		{"isPrivate", "Boolean!"},
		{"isArchived", "Boolean!"},
		{"defaultBranchRef", "Ref"},
		{"issues", "IssueConnection!"},
		{"pullRequests", "PullRequestConnection!"},
		{"issue", "Issue"},
		{"pullRequest", "PullRequest"},
		{"object", "GitObject"},
		{"url", "URI!"},
		{"createdAt", "DateTime!"},
		{"updatedAt", "DateTime!"},
		{"pushedAt", "DateTime!"},
		{"stargazerCount", "Int!"},
	}},
	{Name: "Issue", Interfaces: []string{"Node"}, Fields: [][2]string{
		{"id", "ID!"},
		{"databaseId", "Int"},
		{"number", "Int!"},
		{"title", "String!"},
		{"body", "String!"},
		{"state", "IssueState!"},
		{"createdAt", "DateTime!"},
		{"updatedAt", "DateTime!"},
		{"closedAt", "DateTime"},
		{"url", "URI!"},
		{"author", "User"},
		{"labels", "LabelConnection!"},
		{"comments", "IssueCommentConnection!"},
	}},
	{Name: "PullRequest", Interfaces: []string{"Node"}, Fields: [][2]string{
		{"id", "ID!"},
		{"databaseId", "Int"},
		{"number", "Int!"},
		{"title", "String!"},
		{"body", "String!"},
		{"state", "PullRequestState!"},
		{"createdAt", "DateTime!"},
		{"updatedAt", "DateTime!"},
		{"closedAt", "DateTime"},
		{"merged", "Boolean!"},
		{"mergedAt", "DateTime"},
		{"mergeable", "Boolean"},
		{"headRefName", "String!"},
		{"baseRefName", "String!"},
		{"url", "URI!"},
		{"author", "User"},
		{"labels", "LabelConnection!"},
		{"comments", "IssueCommentConnection!"},
		{"files", "PullRequestFileConnection!"},
	}},
	{Name: "User", Interfaces: []string{"Node"}, Fields: [][2]string{
		{"id", "ID!"},
		{"databaseId", "Int"},
		{"login", "String!"},
		{"name", "String"},
		{"type", "String!"},
		{"url", "URI!"},
		{"avatarUrl", "URI!"},
	}},
	{Name: "Label", Interfaces: []string{"Node"}, Fields: [][2]string{
		{"id", "ID!"},
		{"name", "String!"},
		{"color", "String!"},
		{"description", "String"},
	}},
	{Name: "IssueComment", Interfaces: []string{"Node"}, Fields: [][2]string{
		{"id", "ID!"},
		{"body", "String!"},
		{"createdAt", "DateTime!"},
		{"author", "User"},
	}},
	{Name: "Ref", Fields: [][2]string{
		{"name", "String!"},
		{"prefix", "String!"},
		{"target", "Commit"},
	}},
	{Name: "Commit", Interfaces: []string{"GitObject"}, Fields: [][2]string{
		{"oid", "String!"},
		{"message", "String"},
		{"messageHeadline", "String"},
		{"committedDate", "DateTime"},
		{"tree", "Tree"},
	}},
	{Name: "Tree", Interfaces: []string{"GitObject"}, Fields: [][2]string{
		{"oid", "String!"},
		{"entries", "[TreeEntry!]"},
	}},
	{Name: "TreeEntry", Fields: [][2]string{
		{"name", "String!"},
		{"oid", "String!"},
		{"type", "String!"},
	}},
	{Name: "Blob", Interfaces: []string{"GitObject"}, Fields: [][2]string{
		{"oid", "String!"},
		{"text", "String"},
		{"byteSize", "Int!"},
	}},
	{Name: "__Schema", Fields: [][2]string{
		{"description", "String"},
		{"queryType", "__Type!"},
		{"mutationType", "__Type"},
		{"subscriptionType", "__Type"},
		{"types", "[__Type!]!"},
		{"directives", "[__Directive!]!"},
	}},
	{Name: "__Type", Fields: [][2]string{
		{"kind", "__TypeKind!"},
		{"name", "String"},
		{"description", "String"},
		{"fields", "[__Field!]"},
		{"interfaces", "[__Type!]"},
		{"possibleTypes", "[__Type!]"},
		{"enumValues", "[__EnumValue!]"},
		{"inputFields", "[__InputValue!]"},
		{"ofType", "__Type"},
		{"specifiedByURL", "String"},
	}},
	{Name: "__Field", Fields: [][2]string{
		{"name", "String!"},
		{"description", "String"},
		{"args", "[__InputValue!]!"},
		{"type", "__Type!"},
		{"isDeprecated", "Boolean!"},
		{"deprecationReason", "String"},
	}},
	{Name: "PageInfo", Fields: [][2]string{
		{"hasNextPage", "Boolean!"},
		{"endCursor", "String"},
	}},
	{Name: "IssueConnection", Fields: [][2]string{
		{"totalCount", "Int!"},
		{"pageInfo", "PageInfo!"},
		{"nodes", "[Issue!]"},
	}},
	{Name: "PullRequestConnection", Fields: [][2]string{
		{"totalCount", "Int!"},
		{"pageInfo", "PageInfo!"},
		{"nodes", "[PullRequest!]"},
	}},
	{Name: "IssueCommentConnection", Fields: [][2]string{
		{"totalCount", "Int!"},
		{"pageInfo", "PageInfo!"},
		{"nodes", "[IssueComment!]"},
	}},
	{Name: "LabelConnection", Fields: [][2]string{
		{"totalCount", "Int!"},
		{"pageInfo", "PageInfo!"},
		{"nodes", "[Label!]"},
	}},
	{Name: "PullRequestFileConnection", Fields: [][2]string{
		{"totalCount", "Int!"},
		{"pageInfo", "PageInfo!"},
		{"nodes", "[PullRequestFile!]"},
	}},
	{Name: "PullRequestFile", Fields: [][2]string{
		{"path", "String!"},
		{"additions", "Int!"},
		{"deletions", "Int!"},
	}},
	{Name: "SearchResultItemConnection", Fields: [][2]string{
		{"totalCount", "Int!"},
		{"pageInfo", "PageInfo!"},
	}},
}

var interfaceSources = []interfaceSource{
	{Name: "Node", Fields: [][2]string{{"id", "ID!"}}, PossibleTypes: nodeTypes},
	{Name: "GitObject", Fields: [][2]string{{"oid", "String!"}}, PossibleTypes: gitTypes},
}

var enumSources = []enumSource{
	{Name: "IssueState", Values: []string{"OPEN", "CLOSED"}},
	{Name: "PullRequestState", Values: []string{"OPEN", "CLOSED", "MERGED"}},
	{Name: "SearchType", Values: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"}},
	{Name: "IssueOrderField", Values: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	{Name: "OrderDirection", Values: []string{"ASC", "DESC"}},
	{Name: "RepositoryPermission", Values: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	{Name: "IssueStateReason", Values: []string{"COMPLETED", "NOT_PLANNED", "REOPENED"}},
	{Name: "SubscriptionState", Values: []string{"SUBSCRIBED", "UNSUBSCRIBED", "IGNORED"}},
	{Name: "__TypeKind", Values: []string{"SCALAR", "OBJECT", "INTERFACE", "UNION", "ENUM", "INPUT_OBJECT", "LIST", "NON_NULL"}},
}

func shellOf(name, kind string) *IntroType {
	var namePtr *string
	if name != "" {
		s := name
		namePtr = &s
	}
	return &IntroType{Name: namePtr, Kind: kind}
}

func scalarName(name string) bool {
	for _, candidate := range scalarNames {
		if candidate == name {
			return true
		}
	}
	return false
}

func refTo(text string) *IntroType {
	trimmed := strings.TrimSpace(text)
	if strings.HasSuffix(trimmed, "!") {
		wrap := shellOf("", "NON_NULL")
		wrap.OfType = refTo(trimmed[:len(trimmed)-1])
		return wrap
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		wrap := shellOf("", "LIST")
		wrap.OfType = refTo(trimmed[1 : len(trimmed)-1])
		return wrap
	}
	if known, ok := typeIndex[trimmed]; ok {
		return known
	}
	kind := "OBJECT"
	if scalarName(trimmed) {
		kind = "SCALAR"
	}
	return shellOf(trimmed, kind)
}

func buildIntrospection() {
	typeIndex = map[string]*IntroType{}
	introOrder = nil
	add := func(name string, typ *IntroType) {
		typeIndex[name] = typ
		introOrder = append(introOrder, typ)
	}
	for _, name := range scalarNames {
		add(name, shellOf(name, "SCALAR"))
	}
	for _, item := range enumSources {
		typ := shellOf(item.Name, "ENUM")
		values := make([]map[string]any, 0, len(item.Values))
		for _, value := range item.Values {
			values = append(values, map[string]any{
				"name":              value,
				"description":       nil,
				"isDeprecated":      false,
				"deprecationReason": nil,
			})
		}
		typ.EnumValues = values
		add(item.Name, typ)
	}
	for _, item := range interfaceSources {
		typ := shellOf(item.Name, "INTERFACE")
		typ.Fields = []*IntroFieldRef{}
		typ.PossibleTypes = []*IntroType{}
		add(item.Name, typ)
	}
	for _, item := range objectSources {
		add(item.Name, shellOf(item.Name, "OBJECT"))
	}
	for _, item := range objectSources {
		typ := typeIndex[item.Name]
		typ.Fields = make([]*IntroFieldRef, 0, len(item.Fields))
		for _, pair := range item.Fields {
			typ.Fields = append(typ.Fields, &IntroFieldRef{
				Name:        pair[0],
				Description: item.Name + "." + pair[0],
				Args:        []*IntroArg{},
				Type:        refTo(pair[1]),
			})
		}
		for _, name := range item.Interfaces {
			if found, ok := typeIndex[name]; ok {
				typ.Interfaces = append(typ.Interfaces, found)
			}
		}
	}
	for _, item := range interfaceSources {
		typ := typeIndex[item.Name]
		typ.Fields = make([]*IntroFieldRef, 0, len(item.Fields))
		for _, pair := range item.Fields {
			typ.Fields = append(typ.Fields, &IntroFieldRef{
				Name:        pair[0],
				Description: item.Name + "." + pair[0],
				Args:        []*IntroArg{},
				Type:        refTo(pair[1]),
			})
		}
		for _, name := range item.PossibleTypes {
			if found, ok := typeIndex[name]; ok {
				typ.PossibleTypes = append(typ.PossibleTypes, found)
			}
		}
	}

	objectArguments := map[string][][3]string{
		"Query.repository":        {{"owner", "String!", ""}, {"name", "String!", ""}},
		"Query.node":              {{"id", "ID!", ""}},
		"Query.search":            {{"query", "String!", ""}, {"first", "Int", ""}, {"after", "String", ""}},
		"Query.__type":            {{"name", "String!", ""}},
		"Repository.issues":       {{"first", "Int", ""}, {"after", "String", ""}, {"states", "[IssueState!]", ""}, {"orderBy", "IssueOrder", ""}},
		"Repository.pullRequests": {{"first", "Int", ""}, {"after", "String", ""}, {"states", "[PullRequestState!]", ""}},
		"Repository.issue":        {{"number", "Int!", ""}},
		"Repository.pullRequest":  {{"number", "Int!", ""}},
		"Repository.object":       {{"expression", "String!", ""}},
		"Issue.labels":            {{"first", "Int", ""}, {"after", "String", ""}},
		"Issue.comments":          {{"first", "Int", ""}, {"after", "String", ""}},
		"PullRequest.labels":      {{"first", "Int", ""}, {"after", "String", ""}},
		"PullRequest.comments":    {{"first", "Int", ""}, {"after", "String", ""}},
		"PullRequest.files":       {{"first", "Int", ""}, {"after", "String", ""}},
	}
	for key, list := range objectArguments {
		dot := strings.IndexByte(key, '.')
		typeName := key[:dot]
		fieldName := key[dot+1:]
		typ, ok := typeIndex[typeName]
		if !ok || typ.Fields == nil {
			continue
		}
		for _, field := range typ.Fields {
			if field.Name != fieldName {
				continue
			}
			args := make([]*IntroArg, 0, len(list))
			for _, item := range list {
				args = append(args, &IntroArg{Name: item[0], Description: item[2], Type: refTo(item[1])})
			}
			field.Args = args
		}
	}

	resolvers["__Schema"] = resolverTable[any](map[string]func(any, map[string]any, *ExecCtx) (any, error){
		"description": func(_ any, _ map[string]any, _ *ExecCtx) (any, error) { return nil, nil },
		"queryType": func(_ any, _ map[string]any, _ *ExecCtx) (any, error) {
			return gnode("__Type", typeIndex["Query"]), nil
		},
		"mutationType":     func(_ any, _ map[string]any, _ *ExecCtx) (any, error) { return typeIndex["Mutation"], nil },
		"subscriptionType": func(_ any, _ map[string]any, _ *ExecCtx) (any, error) { return nil, nil },
		"types": func(_ any, _ map[string]any, _ *ExecCtx) (any, error) {
			out := make([]any, 0, len(introOrder))
			for _, typ := range introOrder {
				out = append(out, gnode("__Type", typ))
			}
			return out, nil
		},
		"directives": func(_ any, _ map[string]any, _ *ExecCtx) (any, error) { return []any{}, nil },
	})

	resolvers["__Type"] = resolverTable[*IntroType](map[string]func(*IntroType, map[string]any, *ExecCtx) (any, error){
		"name":        func(typ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) { return typ.Name, nil },
		"kind":        func(typ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) { return typ.Kind, nil },
		"description": func(typ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) { return typ.Description, nil },
		"fields": func(typ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) {
			if typ.Fields == nil {
				return nil, nil
			}
			out := make([]any, 0, len(typ.Fields))
			for _, field := range typ.Fields {
				out = append(out, gnode("__Field", field))
			}
			return out, nil
		},
		"interfaces": func(typ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) {
			out := make([]any, 0, len(typ.Interfaces))
			for _, item := range typ.Interfaces {
				out = append(out, gnode("__Type", item))
			}
			return out, nil
		},
		"possibleTypes": func(typ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) {
			if typ.PossibleTypes == nil {
				return nil, nil
			}
			out := make([]any, 0, len(typ.PossibleTypes))
			for _, item := range typ.PossibleTypes {
				out = append(out, gnode("__Type", item))
			}
			return out, nil
		},
		"enumValues": func(typ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) {
			if typ.EnumValues == nil {
				return nil, nil
			}
			out := make([]any, 0, len(typ.EnumValues))
			for _, item := range typ.EnumValues {
				out = append(out, item)
			}
			return out, nil
		},
		"inputFields": func(_ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) { return nil, nil },
		"ofType": func(typ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) {
			if typ.OfType == nil {
				return nil, nil
			}
			return gnode("__Type", typ.OfType), nil
		},
		"specifiedByURL": func(_ *IntroType, _ map[string]any, _ *ExecCtx) (any, error) { return nil, nil },
	})

	resolvers["__Field"] = resolverTable[*IntroFieldRef](map[string]func(*IntroFieldRef, map[string]any, *ExecCtx) (any, error){
		"name":        func(field *IntroFieldRef, _ map[string]any, _ *ExecCtx) (any, error) { return field.Name, nil },
		"description": func(field *IntroFieldRef, _ map[string]any, _ *ExecCtx) (any, error) { return field.Description, nil },
		"args": func(field *IntroFieldRef, _ map[string]any, _ *ExecCtx) (any, error) {
			out := make([]any, 0, len(field.Args))
			for _, arg := range field.Args {
				out = append(out, map[string]any{
					"name":         arg.Name,
					"description":  arg.Description,
					"defaultValue": arg.DefaultValue,
					"type":         gnode("__Type", arg.Type),
				})
			}
			return out, nil
		},
		"type": func(field *IntroFieldRef, _ map[string]any, _ *ExecCtx) (any, error) {
			return gnode("__Type", field.Type), nil
		},
		"isDeprecated": func(field *IntroFieldRef, _ map[string]any, _ *ExecCtx) (any, error) { return field.IsDeprecated, nil },
		"deprecationReason": func(field *IntroFieldRef, _ map[string]any, _ *ExecCtx) (any, error) {
			return field.DeprecationReason, nil
		},
	})
}

func init() {
	buildResolvers()
	buildIntrospection()
}
