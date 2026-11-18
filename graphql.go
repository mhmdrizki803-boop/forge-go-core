package main

// Dependency-free GraphQL v4 lexer, parser, executor, schema metadata and route.
// Resolvers share the store with REST so both APIs expose identical resources.

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// GNode is a resolved GraphQL object: the type it answers as, plus the record behind it.
type GNode struct {
	Gql string
	V   any
}

func gnode(gql string, v any) *GNode { return &GNode{Gql: gql, V: v} }

func isGNode(value any) (*GNode, bool) {
	node, ok := value.(*GNode)
	return node, ok
}

type GqlError struct {
	Message   string
	Locations []map[string]int
	Path      []any
}

type ExecCtx struct {
	Store     *Store
	Login     string
	User      *UserRecord
	Vars      map[string]any
	Errors    []*GqlError
	Fragments map[string]*FragmentDef
}

// ---------------------------------------------------------------- lexer

type Tok struct {
	Kind  string
	Value string
	Line  int
	Col   int
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNamePart(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func isPunct(c byte) bool {
	switch c {
	case '{', '}', '(', ')', '[', ']', ':', '=', '!', '|', '&', '@':
		return true
	}
	return false
}

func tokenize(src string) ([]Tok, error) {
	toks := []Tok{}
	i := 0
	line := 1
	col := 1
	advance := func(n int) {
		for k := 0; k < n && i < len(src); k++ {
			if src[i] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
			i++
		}
	}
	for i < len(src) {
		ch := src[i]
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == ',' {
			advance(1)
			continue
		}
		if ch == '#' {
			for i < len(src) && src[i] != '\n' {
				advance(1)
			}
			continue
		}
		atLine, atCol := line, col
		if strings.HasPrefix(src[i:], "...") {
			toks = append(toks, Tok{Kind: "punct", Value: "...", Line: atLine, Col: atCol})
			advance(3)
			continue
		}
		if isPunct(ch) {
			toks = append(toks, Tok{Kind: "punct", Value: string(ch), Line: atLine, Col: atCol})
			advance(1)
			continue
		}
		if ch == '"' {
			if strings.HasPrefix(src[i:], `"""`) {
				advance(3)
				text := strings.Builder{}
				for i < len(src) && !strings.HasPrefix(src[i:], `"""`) {
					text.WriteByte(src[i])
					advance(1)
				}
				if i >= len(src) {
					return nil, fmt.Errorf("Unterminated block string at %d:%d", atLine, atCol)
				}
				advance(3)
				toks = append(toks, Tok{Kind: "str", Value: text.String(), Line: atLine, Col: atCol})
				continue
			}
			advance(1)
			text := strings.Builder{}
			closed := false
			for i < len(src) {
				cur := src[i]
				if cur == '"' {
					advance(1)
					closed = true
					break
				}
				if cur == '\\' {
					var next byte
					if i+1 < len(src) {
						next = src[i+1]
					}
					if next == 'u' && i+6 <= len(src) {
						code, err := strconv.ParseUint(src[i+2:i+6], 16, 32)
						if err == nil {
							text.WriteRune(rune(code))
						}
						advance(6)
						continue
					}
					switch next {
					case '"':
						text.WriteByte('"')
					case '\\':
						text.WriteByte('\\')
					case '/':
						text.WriteByte('/')
					case 'b':
						text.WriteByte('\b')
					case 'f':
						text.WriteByte('\f')
					case 'n':
						text.WriteByte('\n')
					case 'r':
						text.WriteByte('\r')
					case 't':
						text.WriteByte('\t')
					default:
						text.WriteByte(next)
					}
					advance(2)
					continue
				}
				text.WriteByte(cur)
				advance(1)
			}
			if !closed {
				return nil, fmt.Errorf("Unterminated string at %d:%d", atLine, atCol)
			}
			toks = append(toks, Tok{Kind: "str", Value: text.String(), Line: atLine, Col: atCol})
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			start := i
			for i < len(src) && (src[i] == '-' || src[i] == '+' || src[i] == '.' || src[i] == 'e' || src[i] == 'E' || (src[i] >= '0' && src[i] <= '9')) {
				advance(1)
			}
			toks = append(toks, Tok{Kind: "num", Value: src[start:i], Line: atLine, Col: atCol})
			continue
		}
		if ch == '$' {
			advance(1)
			start := i
			for i < len(src) && isNamePart(src[i]) {
				advance(1)
			}
			toks = append(toks, Tok{Kind: "var", Value: src[start:i], Line: atLine, Col: atCol})
			continue
		}
		if isNameStart(ch) {
			start := i
			for i < len(src) && isNamePart(src[i]) {
				advance(1)
			}
			toks = append(toks, Tok{Kind: "name", Value: src[start:i], Line: atLine, Col: atCol})
			continue
		}
		return nil, fmt.Errorf("Unexpected character %q at %d:%d", string(ch), atLine, atCol)
	}
	toks = append(toks, Tok{Kind: "eof", Line: line, Col: col})
	return toks, nil
}

// ---------------------------------------------------------------- parser

type Val struct {
	Kind   string
	Name   string
	Value  any
	Values []Val
	Fields map[string]Val
}

type FieldNode struct {
	Alias     string
	HasAlias  bool
	Name      string
	Args      map[string]Val
	Selection []Sel
	Line      int
	Col       int
}

type InlineNode struct {
	TypeCond  string
	Selection []Sel
}

type SpreadNode struct{ Name string }

type Sel any

type FragmentDef struct {
	TypeCond  string
	Selection []Sel
}

type Operation struct {
	Type      string
	Name      string
	HasName   bool
	Defaults  map[string]Val
	Selection []Sel
}

type Parser struct {
	toks []Tok
	i    int
}

func (p *Parser) peek(offset int) Tok {
	index := p.i + offset
	if index >= len(p.toks) {
		index = len(p.toks) - 1
	}
	return p.toks[index]
}

func (p *Parser) next() Tok {
	tok := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return tok
}

func (p *Parser) atPunct(value string) bool {
	tok := p.peek(0)
	return tok.Kind == "punct" && tok.Value == value
}

func (p *Parser) atName(value string) bool {
	tok := p.peek(0)
	return tok.Kind == "name" && tok.Value == value
}

func (p *Parser) expectPunct(value string) error {
	tok := p.next()
	if tok.Kind == "punct" && tok.Value == value {
		return nil
	}
	return fmt.Errorf("Expected %q but found %s", value, describeTok(tok))
}

func (p *Parser) expectName() (Tok, error) {
	tok := p.next()
	if tok.Kind == "name" {
		return tok, nil
	}
	return tok, fmt.Errorf("Expected a name but found %s", describeTok(tok))
}

func describeTok(tok Tok) string {
	switch tok.Kind {
	case "eof":
		return "end of document"
	case "var":
		return `"$` + tok.Value + `"`
	case "str":
		return "a string"
	}
	return `"` + tok.Value + `"`
}

func parseDocument(src string) ([]*Operation, map[string]*FragmentDef, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, nil, err
	}
	p := &Parser{toks: toks}
	operations := []*Operation{}
	fragments := map[string]*FragmentDef{}
	for p.peek(0).Kind != "eof" {
		if p.atPunct("{") {
			selection, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			operations = append(operations, &Operation{Type: "query", Defaults: map[string]Val{}, Selection: selection})
			continue
		}
		keywordTok, err := p.expectName()
		if err != nil {
			return nil, nil, err
		}
		keyword := keywordTok.Value
		if keyword == "fragment" {
			nameTok, err := p.expectName()
			if err != nil {
				return nil, nil, err
			}
			typeCond := ""
			if p.atName("on") {
				p.next()
				condTok, err := p.expectName()
				if err != nil {
					return nil, nil, err
				}
				typeCond = condTok.Value
			}
			if err := p.skipDirectives(); err != nil {
				return nil, nil, err
			}
			selection, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			fragments[nameTok.Value] = &FragmentDef{TypeCond: typeCond, Selection: selection}
			continue
		}
		if keyword != "query" && keyword != "mutation" && keyword != "subscription" {
			return nil, nil, fmt.Errorf("Unsupported operation %q", keyword)
		}
		operation := &Operation{Type: keyword, Defaults: map[string]Val{}}
		if p.peek(0).Kind == "name" {
			operation.Name = p.next().Value
			operation.HasName = true
		}
		if p.atPunct("(") {
			p.next()
			for !p.atPunct(")") {
				if p.peek(0).Kind == "eof" {
					return nil, nil, fmt.Errorf("Unterminated variable definitions")
				}
				tok := p.next()
				if tok.Kind != "var" {
					return nil, nil, fmt.Errorf("Expected a variable but found %s", describeTok(tok))
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, nil, err
				}
				if err := p.parseTypeRef(); err != nil {
					return nil, nil, err
				}
				if p.atPunct("=") {
					p.next()
					value, err := p.parseValue()
					if err != nil {
						return nil, nil, err
					}
					operation.Defaults[tok.Value] = value
				}
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, nil, err
			}
		}
		if err := p.skipDirectives(); err != nil {
			return nil, nil, err
		}
		selection, err := p.parseSelectionSet()
		if err != nil {
			return nil, nil, err
		}
		operation.Selection = selection
		operations = append(operations, operation)
	}
	return operations, fragments, nil
}

func (p *Parser) parseTypeRef() error {
	if p.atPunct("[") {
		p.next()
		if err := p.parseTypeRef(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	if p.atPunct("!") {
		p.next()
	}
	return nil
}

func (p *Parser) skipDirectives() error {
	for p.atPunct("@") {
		p.next()
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.atPunct("(") {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Parser) parseSelectionSet() ([]Sel, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	selection := []Sel{}
	for !p.atPunct("}") {
		if p.peek(0).Kind == "eof" {
			return nil, fmt.Errorf("Unterminated selection set")
		}
		item, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selection = append(selection, item)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	if len(selection) == 0 {
		return nil, fmt.Errorf("A selection set may not be empty")
	}
	return selection, nil
}

func (p *Parser) parseSelection() (Sel, error) {
	if p.atPunct("...") {
		p.next()
		if p.atName("on") {
			p.next()
			condTok, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			selection, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &InlineNode{TypeCond: condTok.Value, Selection: selection}, nil
		}
		if p.atPunct("{") {
			selection, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &InlineNode{Selection: selection}, nil
		}
		nameTok, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &SpreadNode{Name: nameTok.Value}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	field := &FieldNode{Name: first.Value, Args: map[string]Val{}, Line: first.Line, Col: first.Col}
	if p.atPunct(":") {
		p.next()
		field.Alias = field.Name
		field.HasAlias = true
		nameTok, err := p.expectName()
		if err != nil {
			return nil, err
		}
		field.Name = nameTok.Value
	}
	if p.atPunct("(") {
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		field.Args = args
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	if p.atPunct("{") {
		selection, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		field.Selection = selection
	}
	return field, nil
}

func (p *Parser) parseArguments() (map[string]Val, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	args := map[string]Val{}
	for !p.atPunct(")") {
		if p.peek(0).Kind == "eof" {
			return nil, fmt.Errorf("Unterminated arguments")
		}
		nameTok, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[nameTok.Value] = value
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *Parser) parseValue() (Val, error) {
	tok := p.next()
	switch tok.Kind {
	case "var":
		return Val{Kind: "var", Name: tok.Value}, nil
	case "num":
		if strings.ContainsAny(tok.Value, ".eE") {
			f, _ := strconv.ParseFloat(tok.Value, 64)
			return Val{Kind: "float", Value: f}, nil
		}
		n, _ := strconv.Atoi(tok.Value)
		return Val{Kind: "int", Value: n}, nil
	case "str":
		return Val{Kind: "string", Value: tok.Value}, nil
	case "name":
		switch tok.Value {
		case "true":
			return Val{Kind: "bool", Value: true}, nil
		case "false":
			return Val{Kind: "bool", Value: false}, nil
		case "null":
			return Val{Kind: "null"}, nil
		}
		return Val{Kind: "enum", Value: tok.Value}, nil
	case "punct":
		if tok.Value == "[" {
			values := []Val{}
			for !p.atPunct("]") {
				item, err := p.parseValue()
				if err != nil {
					return Val{}, err
				}
				values = append(values, item)
			}
			if err := p.expectPunct("]"); err != nil {
				return Val{}, err
			}
			return Val{Kind: "list", Values: values}, nil
		}
		if tok.Value == "{" {
			fields := map[string]Val{}
			for !p.atPunct("}") {
				nameTok, err := p.expectName()
				if err != nil {
					return Val{}, err
				}
				if err := p.expectPunct(":"); err != nil {
					return Val{}, err
				}
				item, err := p.parseValue()
				if err != nil {
					return Val{}, err
				}
				fields[nameTok.Value] = item
			}
			if err := p.expectPunct("}"); err != nil {
				return Val{}, err
			}
			return Val{Kind: "object", Fields: fields}, nil
		}
	}
	return Val{}, fmt.Errorf("Expected a value but found %s", describeTok(tok))
}

// ---------------------------------------------------------------- value evaluation

func evalValue(val Val, ec *ExecCtx) (any, error) {
	switch val.Kind {
	case "var":
		if v, ok := ec.Vars[val.Name]; ok {
			return v, nil
		}
		return nil, fmt.Errorf("Variable \"$%s\" was not provided.", val.Name)
	case "list":
		out := make([]any, 0, len(val.Values))
		for _, item := range val.Values {
			v, err := evalValue(item, ec)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case "object":
		out := map[string]any{}
		for key, item := range val.Fields {
			v, err := evalValue(item, ec)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		return out, nil
	}
	return val.Value, nil
}

func evalArgs(args map[string]Val, ec *ExecCtx) (map[string]any, error) {
	out := map[string]any{}
	if len(args) == 1 {
		if input, ok := args["input"]; ok && input.Kind == "var" {
			if _, present := ec.Vars[input.Name]; !present {
				if len(ec.Vars) == 1 {
					for _, v := range ec.Vars {
						if m, ok := v.(map[string]any); ok {
							out["input"] = m
							return out, nil
						}
					}
				}
			}
		}
	}
	for key, val := range args {
		v, err := evalValue(val, ec)
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}

// ---------------------------------------------------------------- pagination

func encodeCursor(prefix string, index int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + prefix + ":" + strconv.Itoa(index)))
}

func decodeCursor(value any, prefix string) (int, bool) {
	s, ok := value.(string)
	if !ok || s == "" {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	rest := text[len("cursor:"):]
	colon := strings.LastIndexByte(rest, ':')
	if colon < 0 {
		return 0, false
	}
	if rest[:colon] != prefix {
		return 0, false
	}
	n, err := strconv.Atoi(rest[colon+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

func connection(nodes []any, args map[string]any, prefix string) map[string]any {
	start := 0
	if after, ok := decodeCursor(args["after"], prefix); ok {
		start = after + 1
	}
	first := len(nodes)
	if f, ok := asInt(args["first"]); ok && f >= 0 {
		first = f
	}
	end := start + first
	if start > len(nodes) {
		start = len(nodes)
	}
	if end > len(nodes) {
		end = len(nodes)
	}
	if end < start {
		end = start
	}
	slice := nodes[start:end]
	hasNext := start+len(slice) < len(nodes)
	var endCursor any
	if len(slice) == 0 {
		endCursor = nil
	} else {
		endCursor = encodeCursor(prefix, start+len(slice)-1)
	}
	return map[string]any{
		"totalCount": len(nodes),
		"pageInfo":   map[string]any{"hasNextPage": hasNext, "endCursor": endCursor},
		"nodes":      slice,
	}
}

func asInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func asStr(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func asMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return nil
}

func asSlice(value any) ([]any, bool) {
	s, ok := value.([]any)
	return s, ok
}

// ---------------------------------------------------------------- git helpers

func commitTreeOf(store *Store, repo *RepoRecord, oid string) string {
	if oid == "" {
		return ""
	}
	object := store.GetObject(repo, oid)
	if object == nil || object.Type != "commit" {
		return ""
	}
	for _, line := range strings.Split(string(object.Body), "\n") {
		if strings.HasPrefix(line, "tree ") {
			return strings.TrimSpace(line[5:])
		}
		if line == "" {
			break
		}
	}
	return ""
}

func flattenTreeSimple(store *Store, repo *RepoRecord, oid, prefix string, out map[string]string) {
	if oid == "" {
		return
	}
	object := store.GetObject(repo, oid)
	if object == nil || object.Type != "tree" {
		return
	}
	body := object.Body
	at := 0
	for at < len(body) {
		space := indexByteFrom(body, ' ', at)
		if space < 0 {
			break
		}
		nul := indexByteFrom(body, 0, space+1)
		if nul < 0 || nul+21 > len(body) {
			break
		}
		mode := string(body[at:space])
		name := string(body[space+1 : nul])
		child := hexEncode(body[nul+1 : nul+21])
		path := name
		if prefix != "" {
			path = prefix + "/" + name
		}
		if mode == modeDir {
			flattenTreeSimple(store, repo, child, path, out)
		} else {
			out[path] = child
		}
		at = nul + 21
	}
}

func blobBodyOf(store *Store, repo *RepoRecord, oid string) []byte {
	if oid == "" {
		return nil
	}
	object := store.GetObject(repo, oid)
	if object != nil && object.Type == "blob" {
		return object.Body
	}
	return nil
}

func lineDiffBytes(before, after []byte) (int, int) {
	counts := map[string]int{}
	for _, line := range linesOf(before) {
		counts[line]++
	}
	additions := 0
	for _, line := range linesOf(after) {
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

// ---------------------------------------------------------------- node payloads

type issueNodePayload struct {
	Rec  *IssueRecord
	Repo *RepoRecord
}

type pullNodePayload struct {
	Rec  *PullRecord
	Repo *RepoRecord
}

// ---------------------------------------------------------------- resolvers

type Resolver func(v any, args map[string]any, ec *ExecCtx) (any, error)

func uppercaseState(state string) string {
	if state == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func pullStateOf(pull *PullRecord) string {
	if pull.Merged {
		return "MERGED"
	}
	return uppercaseState(pull.State)
}

func filterStatesIssues(records []*IssueRecord, states any) []*IssueRecord {
	list, ok := asSlice(states)
	if !ok || len(list) == 0 {
		return records
	}
	wanted := map[string]bool{}
	for _, state := range list {
		wanted[strings.ToUpper(fmt.Sprint(state))] = true
	}
	out := []*IssueRecord{}
	for _, record := range records {
		if wanted[uppercaseState(record.State)] {
			out = append(out, record)
		}
	}
	return out
}

func filterStatesPulls(records []*PullRecord, states any) []*PullRecord {
	list, ok := asSlice(states)
	if !ok || len(list) == 0 {
		return records
	}
	wanted := map[string]bool{}
	for _, state := range list {
		wanted[strings.ToUpper(fmt.Sprint(state))] = true
	}
	out := []*PullRecord{}
	for _, record := range records {
		if wanted[pullStateOf(record)] {
			out = append(out, record)
		}
	}
	return out
}

func byNumberAscIssues(records []*IssueRecord, orderBy any) []*IssueRecord {
	sorted := make([]*IssueRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	direction := "ASC"
	if obj := asMap(orderBy); obj != nil {
		if d, ok := obj["direction"]; ok {
			direction = strings.ToUpper(fmt.Sprint(d))
		}
	}
	if direction == "DESC" {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	return sorted
}

func byNumberAscPulls(records []*PullRecord, orderBy any) []*PullRecord {
	sorted := make([]*PullRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	direction := "ASC"
	if obj := asMap(orderBy); obj != nil {
		if d, ok := obj["direction"]; ok {
			direction = strings.ToUpper(fmt.Sprint(d))
		}
	}
	if direction == "DESC" {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	return sorted
}

var resolvers = map[string]map[string]Resolver{}

// resolverTable adapts a table of concretely typed resolvers into the dynamic table.
func resolverTable[T any](table map[string]func(T, map[string]any, *ExecCtx) (any, error)) map[string]Resolver {
	out := map[string]Resolver{}
	for name, fn := range table {
		function := fn
		label := name
		out[name] = func(v any, args map[string]any, ec *ExecCtx) (any, error) {
			typed, ok := v.(T)
			if !ok {
				return nil, fmt.Errorf("resolver %s got an unexpected payload", label)
			}
			return function(typed, args, ec)
		}
	}
	return out
}

func addResolver(typeName, field string, fn Resolver) {
	table, ok := resolvers[typeName]
	if !ok {
		table = map[string]Resolver{}
		resolvers[typeName] = table
	}
	table[field] = fn
}

func buildResolvers() {
	resolvers["Query"] = resolverTable[any](map[string]func(any, map[string]any, *ExecCtx) (any, error){
		"viewer": func(_ any, _ map[string]any, ec *ExecCtx) (any, error) {
			if ec.User == nil {
				return nil, nil
			}
			return gnode("User", ec.User), nil
		},
		"repository": func(_ any, args map[string]any, ec *ExecCtx) (any, error) {
			repo := ec.Store.GetRepo(asStr(args["owner"]), asStr(args["name"]))
			if repo == nil {
				return nil, nil
			}
			return gnode("Repository", repo), nil
		},
		"node": func(_ any, args map[string]any, ec *ExecCtx) (any, error) {
			return decodeGlobalID(ec.Store, asStr(args["id"]))
		},
		"search": func(_ any, args map[string]any, _ *ExecCtx) (any, error) {
			return connection([]any{}, args, "s"), nil
		},
		"__schema": func(_ any, _ map[string]any, _ *ExecCtx) (any, error) {
			return gnode("__Schema", map[string]any{}), nil
		},
		"__type": func(_ any, args map[string]any, _ *ExecCtx) (any, error) {
			found, ok := typeIndex[asStr(args["name"])]
			if !ok {
				return nil, nil
			}
			return gnode("__Type", found), nil
		},
	})

	resolvers["User"] = resolverTable[*UserRecord](map[string]func(*UserRecord, map[string]any, *ExecCtx) (any, error){
		"id":         func(user *UserRecord, _ map[string]any, _ *ExecCtx) (any, error) { return user.NodeID, nil },
		"login":      func(user *UserRecord, _ map[string]any, _ *ExecCtx) (any, error) { return user.Login, nil },
		"name":       func(user *UserRecord, _ map[string]any, _ *ExecCtx) (any, error) { return user.Name, nil },
		"type":       func(user *UserRecord, _ map[string]any, _ *ExecCtx) (any, error) { return user.Type, nil },
		"url":        func(user *UserRecord, _ map[string]any, _ *ExecCtx) (any, error) { return user.HTMLURL, nil },
		"avatarUrl":  func(user *UserRecord, _ map[string]any, _ *ExecCtx) (any, error) { return user.AvatarURL, nil },
		"databaseId": func(user *UserRecord, _ map[string]any, _ *ExecCtx) (any, error) { return user.ID, nil },
	})

	resolvers["Repository"] = resolverTable[*RepoRecord](map[string]func(*RepoRecord, map[string]any, *ExecCtx) (any, error){
		"id":               func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.NodeID, nil },
		"databaseId":       func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.ID, nil },
		"viewerPermission": func(_ *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return "ADMIN", nil },
		"hasIssuesEnabled": func(_ *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return true, nil },
		"hasWikiEnabled":   func(_ *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return false, nil },
		"isEmpty": func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) {
			return len(repo.Data.Refs) == 0, nil
		},
		"name":           func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.Name, nil },
		"nameWithOwner":  func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.FullName, nil },
		"description":    func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return sp(repo.Description), nil },
		"isPrivate":      func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.Private, nil },
		"isArchived":     func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.Archived, nil },
		"url":            func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.HTMLURL, nil },
		"createdAt":      func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.CreatedAt, nil },
		"updatedAt":      func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.UpdatedAt, nil },
		"pushedAt":       func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return repo.PushedAt, nil },
		"stargazerCount": func(repo *RepoRecord, _ map[string]any, _ *ExecCtx) (any, error) { return 0, nil },
		"owner": func(repo *RepoRecord, _ map[string]any, ec *ExecCtx) (any, error) {
			user := ec.Store.GetUser(repo.OwnerLogin)
			if user == nil {
				return gnode("User", &UserRecord{Login: repo.OwnerLogin}), nil
			}
			return gnode("User", user), nil
		},
		"defaultBranchRef": func(repo *RepoRecord, _ map[string]any, ec *ExecCtx) (any, error) {
			oid, ok := ec.Store.GetRefOk(repo, "refs/heads/"+repo.DefaultBranch)
			var value any
			if ok {
				value = oid
			}
			return gnode("Ref", map[string]any{"name": repo.DefaultBranch, "oid": value, "repo": repo}), nil
		},
		"issues": func(repo *RepoRecord, args map[string]any, _ *ExecCtx) (any, error) {
			all := []*IssueRecord{}
			for _, issue := range repo.Data.Issues {
				all = append(all, issue)
			}
			filtered := filterStatesIssues(all, args["states"])
			ordered := byNumberAscIssues(filtered, args["orderBy"])
			nodes := make([]any, 0, len(ordered))
			for _, issue := range ordered {
				nodes = append(nodes, gnode("Issue", &issueNodePayload{Rec: issue, Repo: repo}))
			}
			return connection(nodes, args, "i"), nil
		},
		"pullRequests": func(repo *RepoRecord, args map[string]any, _ *ExecCtx) (any, error) {
			all := []*PullRecord{}
			for _, pull := range repo.Data.Pulls {
				all = append(all, pull)
			}
			filtered := filterStatesPulls(all, args["states"])
			ordered := byNumberAscPulls(filtered, args["orderBy"])
			nodes := make([]any, 0, len(ordered))
			for _, pull := range ordered {
				nodes = append(nodes, gnode("PullRequest", &pullNodePayload{Rec: pull, Repo: repo}))
			}
			return connection(nodes, args, "p"), nil
		},
		"issue": func(repo *RepoRecord, args map[string]any, ec *ExecCtx) (any, error) {
			number, _ := asInt(args["number"])
			issue := ec.Store.GetIssue(repo, number)
			if issue == nil {
				return nil, fmt.Errorf("Could not resolve to an Issue or PullRequest with the number of %d.", number)
			}
			return gnode("Issue", &issueNodePayload{Rec: issue, Repo: repo}), nil
		},
		"pullRequest": func(repo *RepoRecord, args map[string]any, ec *ExecCtx) (any, error) {
			number, _ := asInt(args["number"])
			pull := ec.Store.GetPull(repo, number)
			if pull == nil {
				return nil, fmt.Errorf("Could not resolve to an Issue or PullRequest with the number of %d.", number)
			}
			return gnode("PullRequest", &pullNodePayload{Rec: pull, Repo: repo}), nil
		},
		"object": func(repo *RepoRecord, args map[string]any, ec *ExecCtx) (any, error) {
			return resolveObjectExpression(ec.Store, repo, asStr(args["expression"]))
		},
	})

	resolvers["Mutation"] = resolverTable[any](map[string]func(any, map[string]any, *ExecCtx) (any, error){
		"createIssue": func(_ any, a map[string]any, ec *ExecCtx) (any, error) {
			input := asMap(a["input"])
			if input == nil {
				input = map[string]any{}
			}
			repo, err := repoByID(ec, asStr(input["repositoryId"]))
			if err != nil {
				return nil, err
			}
			title := asStr(input["title"])
			if strings.TrimSpace(title) == "" {
				return nil, fmt.Errorf("title cannot be blank")
			}
			labels := []string{}
			if list, ok := asSlice(input["labelIds"]); ok {
				for _, item := range list {
					labels = append(labels, fmt.Sprint(item))
				}
			}
			issue := ec.Store.CreateIssue(repo, title, asStr(input["body"]), ec.Login, "open", labels, false)
			return map[string]any{"issue": gnode("Issue", &issueNodePayload{Rec: issue, Repo: repo})}, nil
		},
		"closeIssue": func(_ any, a map[string]any, ec *ExecCtx) (any, error) {
			input := asMap(a["input"])
			found, err := issueByID(ec, asStr(input["issueId"]))
			if err != nil {
				return nil, err
			}
			state := "closed"
			next := ec.Store.UpdateIssue(found.Repo, found.Rec.Number, UpdateIssuePatch{State: &state})
			if next == nil {
				next = found.Rec
			}
			return map[string]any{"issue": gnode("Issue", &issueNodePayload{Rec: next, Repo: found.Repo})}, nil
		},
		"reopenIssue": func(_ any, a map[string]any, ec *ExecCtx) (any, error) {
			input := asMap(a["input"])
			found, err := issueByID(ec, asStr(input["issueId"]))
			if err != nil {
				return nil, err
			}
			state := "open"
			next := ec.Store.UpdateIssue(found.Repo, found.Rec.Number, UpdateIssuePatch{State: &state})
			if next == nil {
				next = found.Rec
			}
			return map[string]any{"issue": gnode("Issue", &issueNodePayload{Rec: next, Repo: found.Repo})}, nil
		},
		"addComment": func(_ any, a map[string]any, ec *ExecCtx) (any, error) {
			input := asMap(a["input"])
			found, err := issueByID(ec, asStr(input["subjectId"]))
			if err != nil {
				return nil, err
			}
			comment := ec.Store.CreateComment(found.Repo, found.Rec.Number, asStr(input["body"]), ec.Login)
			if comment == nil {
				return nil, fmt.Errorf("Could not add the comment.")
			}
			return map[string]any{
				"issueComment": gnode("IssueComment", comment),
				"commentEdge": map[string]any{
					"node":   gnode("IssueComment", comment),
					"cursor": base64.StdEncoding.EncodeToString([]byte("c" + strconv.Itoa(comment.ID))),
				},
			}, nil
		},
		"createPullRequest": func(_ any, a map[string]any, ec *ExecCtx) (any, error) {
			input := asMap(a["input"])
			repo, err := repoByID(ec, asStr(input["repositoryId"]))
			if err != nil {
				return nil, err
			}
			headRef := asStr(input["headRefName"])
			baseRef := asStr(input["baseRefName"])
			if baseRef == "" {
				baseRef = repo.DefaultBranch
			}
			headSha, _ := ec.Store.GetRefOk(repo, "refs/heads/"+headRef)
			baseSha, ok := ec.Store.GetRefOk(repo, "refs/heads/"+baseRef)
			if !ok {
				baseSha = headSha
			}
			pull := ec.Store.CreatePull(repo, asStr(input["title"]), asStr(input["body"]), ec.Login, headRef, headSha, baseRef, baseSha)
			return map[string]any{"pullRequest": gnode("PullRequest", &pullNodePayload{Rec: pull, Repo: repo})}, nil
		},
		"mergePullRequest": func(_ any, a map[string]any, ec *ExecCtx) (any, error) {
			input := asMap(a["input"])
			found, err := pullByID(ec, asStr(input["pullRequestId"]))
			if err != nil {
				return nil, err
			}
			next := ec.Store.MergePull(found.Repo, found.Rec.Number, found.Rec.Head.Sha)
			if next == nil {
				next = found.Rec
			}
			return map[string]any{"pullRequest": gnode("PullRequest", &pullNodePayload{Rec: next, Repo: found.Repo})}, nil
		},
	})

	resolvers["Issue"] = resolverTable[*issueNodePayload](map[string]func(*issueNodePayload, map[string]any, *ExecCtx) (any, error){
		"id":         func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.NodeID, nil },
		"databaseId": func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.ID, nil },
		"number":     func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Number, nil },
		"title":      func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Title, nil },
		"body":       func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Body, nil },
		"state": func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) {
			return uppercaseState(v.Rec.State), nil
		},
		"createdAt": func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.CreatedAt, nil },
		"updatedAt": func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.UpdatedAt, nil },
		"closedAt":  func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return sp(v.Rec.ClosedAt), nil },
		"url":       func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.HTMLURL, nil },
		"author": func(v *issueNodePayload, _ map[string]any, _ *ExecCtx) (any, error) {
			return gnode("User", v.Rec.User), nil
		},
		"labels": func(v *issueNodePayload, args map[string]any, _ *ExecCtx) (any, error) {
			nodes := make([]any, 0, len(v.Rec.Labels))
			for _, label := range v.Rec.Labels {
				nodes = append(nodes, gnode("Label", label))
			}
			return connection(nodes, args, "l"), nil
		},
		"comments": func(v *issueNodePayload, args map[string]any, ec *ExecCtx) (any, error) {
			list := ec.Store.ListComments(v.Repo, v.Rec.Number, true)
			nodes := make([]any, 0, len(list))
			for _, comment := range list {
				nodes = append(nodes, gnode("IssueComment", comment))
			}
			return connection(nodes, args, "c"), nil
		},
	})

	resolvers["PullRequest"] = resolverTable[*pullNodePayload](map[string]func(*pullNodePayload, map[string]any, *ExecCtx) (any, error){
		"id":          func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.NodeID, nil },
		"databaseId":  func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.ID, nil },
		"number":      func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Number, nil },
		"title":       func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Title, nil },
		"body":        func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Body, nil },
		"state":       func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return pullStateOf(v.Rec), nil },
		"createdAt":   func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.CreatedAt, nil },
		"updatedAt":   func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.UpdatedAt, nil },
		"closedAt":    func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return sp(v.Rec.ClosedAt), nil },
		"merged":      func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Merged, nil },
		"mergedAt":    func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return sp(v.Rec.MergedAt), nil },
		"headRefName": func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Head.Ref, nil },
		"baseRefName": func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.Base.Ref, nil },
		"url":         func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) { return v.Rec.HTMLURL, nil },
		"author": func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) {
			return gnode("User", v.Rec.User), nil
		},
		"labels": func(_ *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) {
			return connection([]any{}, map[string]any{}, "l"), nil
		},
		"mergeable": func(v *pullNodePayload, _ map[string]any, _ *ExecCtx) (any, error) {
			if v.Rec.Mergeable != nil && *v.Rec.Mergeable {
				return "MERGEABLE", nil
			}
			return "CONFLICTING", nil
		},
		"comments": func(v *pullNodePayload, args map[string]any, ec *ExecCtx) (any, error) {
			list := ec.Store.ListComments(v.Repo, v.Rec.Number, true)
			nodes := make([]any, 0, len(list))
			for _, comment := range list {
				nodes = append(nodes, gnode("IssueComment", comment))
			}
			return connection(nodes, args, "c"), nil
		},
		"files": func(v *pullNodePayload, args map[string]any, ec *ExecCtx) (any, error) {
			before := map[string]string{}
			after := map[string]string{}
			flattenTreeSimple(ec.Store, v.Repo, commitTreeOf(ec.Store, v.Repo, v.Rec.Base.Sha), "", before)
			flattenTreeSimple(ec.Store, v.Repo, commitTreeOf(ec.Store, v.Repo, v.Rec.Head.Sha), "", after)
			pathSet := map[string]bool{}
			paths := []string{}
			for _, m := range []map[string]string{before, after} {
				for path := range m {
					if !pathSet[path] {
						pathSet[path] = true
						paths = append(paths, path)
					}
				}
			}
			sort.Strings(paths)
			nodes := []any{}
			for _, path := range paths {
				oldOid := before[path]
				newOid := after[path]
				if oldOid == newOid {
					continue
				}
				additions, deletions := lineDiffBytes(blobBodyOf(ec.Store, v.Repo, oldOid), blobBodyOf(ec.Store, v.Repo, newOid))
				nodes = append(nodes, gnode("PullRequestFile", map[string]any{"path": path, "additions": additions, "deletions": deletions}))
			}
			return connection(nodes, args, "f"), nil
		},
	})

	resolvers["Label"] = resolverTable[*LabelRecord](map[string]func(*LabelRecord, map[string]any, *ExecCtx) (any, error){
		"id":          func(label *LabelRecord, _ map[string]any, _ *ExecCtx) (any, error) { return label.NodeID, nil },
		"name":        func(label *LabelRecord, _ map[string]any, _ *ExecCtx) (any, error) { return label.Name, nil },
		"color":       func(label *LabelRecord, _ map[string]any, _ *ExecCtx) (any, error) { return label.Color, nil },
		"description": func(label *LabelRecord, _ map[string]any, _ *ExecCtx) (any, error) { return sp(label.Description), nil },
	})

	resolvers["IssueComment"] = resolverTable[*CommentRecord](map[string]func(*CommentRecord, map[string]any, *ExecCtx) (any, error){
		"id":        func(comment *CommentRecord, _ map[string]any, _ *ExecCtx) (any, error) { return comment.NodeID, nil },
		"body":      func(comment *CommentRecord, _ map[string]any, _ *ExecCtx) (any, error) { return comment.Body, nil },
		"createdAt": func(comment *CommentRecord, _ map[string]any, _ *ExecCtx) (any, error) { return comment.CreatedAt, nil },
		"author": func(comment *CommentRecord, _ map[string]any, _ *ExecCtx) (any, error) {
			return gnode("User", comment.User), nil
		},
	})

	resolvers["Ref"] = resolverTable[map[string]any](map[string]func(map[string]any, map[string]any, *ExecCtx) (any, error){
		"name":   func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["name"], nil },
		"prefix": func(_ map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return "refs/heads/", nil },
		"target": func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) {
			oid, _ := v["oid"].(string)
			if oid == "" {
				return nil, nil
			}
			return gnode("Commit", map[string]any{"oid": oid, "repo": v["repo"]}), nil
		},
	})

	resolvers["Commit"] = resolverTable[map[string]any](map[string]func(map[string]any, map[string]any, *ExecCtx) (any, error){
		"oid":           func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["oid"], nil },
		"committedDate": func(_ map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return nil, nil },
		"message": func(v map[string]any, _ map[string]any, ec *ExecCtx) (any, error) {
			return commitField(ec.Store, v), nil
		},
		"messageHeadline": func(v map[string]any, _ map[string]any, ec *ExecCtx) (any, error) {
			text, _ := commitField(ec.Store, v).(string)
			return strings.Split(text, "\n")[0], nil
		},
		"tree": func(v map[string]any, _ map[string]any, ec *ExecCtx) (any, error) {
			repo, _ := v["repo"].(*RepoRecord)
			tree := commitTreeOf(ec.Store, repo, asStr(v["oid"]))
			if tree == "" {
				return nil, nil
			}
			return gnode("Tree", map[string]any{"oid": tree, "repo": repo}), nil
		},
	})

	resolvers["Tree"] = resolverTable[map[string]any](map[string]func(map[string]any, map[string]any, *ExecCtx) (any, error){
		"oid": func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["oid"], nil },
		"entries": func(v map[string]any, _ map[string]any, ec *ExecCtx) (any, error) {
			repo, _ := v["repo"].(*RepoRecord)
			out := []any{}
			for _, entry := range listTree(ec.Store, repo, asStr(v["oid"])) {
				typ := "blob"
				if entry.Mode == modeDir {
					typ = "tree"
				}
				out = append(out, gnode("TreeEntry", map[string]any{"name": entry.Name, "oid": entry.Oid, "type": typ}))
			}
			return out, nil
		},
	})

	resolvers["TreeEntry"] = resolverTable[map[string]any](map[string]func(map[string]any, map[string]any, *ExecCtx) (any, error){
		"name": func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["name"], nil },
		"oid":  func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["oid"], nil },
		"type": func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["type"], nil },
	})

	resolvers["Blob"] = resolverTable[map[string]any](map[string]func(map[string]any, map[string]any, *ExecCtx) (any, error){
		"oid": func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["oid"], nil },
		"text": func(v map[string]any, _ map[string]any, ec *ExecCtx) (any, error) {
			repo, _ := v["repo"].(*RepoRecord)
			return string(blobBodyOf(ec.Store, repo, asStr(v["oid"]))), nil
		},
		"byteSize": func(v map[string]any, _ map[string]any, ec *ExecCtx) (any, error) {
			repo, _ := v["repo"].(*RepoRecord)
			return len(blobBodyOf(ec.Store, repo, asStr(v["oid"]))), nil
		},
	})

	resolvers["PullRequestFile"] = resolverTable[map[string]any](map[string]func(map[string]any, map[string]any, *ExecCtx) (any, error){
		"path":      func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["path"], nil },
		"additions": func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["additions"], nil },
		"deletions": func(v map[string]any, _ map[string]any, _ *ExecCtx) (any, error) { return v["deletions"], nil },
	})
}

func commitField(store *Store, v map[string]any) any {
	repo, _ := v["repo"].(*RepoRecord)
	object := store.GetObject(repo, asStr(v["oid"]))
	if object == nil || object.Type != "commit" {
		return nil
	}
	raw := string(object.Body)
	split := strings.Index(raw, "\n\n")
	if split < 0 {
		return ""
	}
	return strings.TrimRight(raw[split+2:], "\n")
}

type treeListEntry struct {
	Mode string
	Name string
	Oid  string
}

func listTree(store *Store, repo *RepoRecord, oid string) []treeListEntry {
	object := store.GetObject(repo, oid)
	if object == nil || object.Type != "tree" {
		return nil
	}
	body := object.Body
	entries := []treeListEntry{}
	at := 0
	for at < len(body) {
		space := indexByteFrom(body, ' ', at)
		if space < 0 {
			break
		}
		nul := indexByteFrom(body, 0, space+1)
		if nul < 0 || nul+21 > len(body) {
			break
		}
		entries = append(entries, treeListEntry{
			Mode: string(body[at:space]),
			Name: string(body[space+1 : nul]),
			Oid:  hexEncode(body[nul+1 : nul+21]),
		})
		at = nul + 21
	}
	return entries
}

// resolveObjectExpression resolves `main`, `main:README.md`, `HEAD:src` or a full oid.
func resolveObjectExpression(store *Store, repo *RepoRecord, expression string) (any, error) {
	if expression == "" {
		return nil, nil
	}
	colon := strings.IndexByte(expression, ':')
	ref := expression
	path := ""
	if colon >= 0 {
		ref = expression[:colon]
		path = expression[colon+1:]
	}
	oid, ok := store.ResolveRef(repo, ref)
	if !ok {
		if resolved, _, found := store.ResolveObject(repo, ref); found {
			oid = resolved
			ok = true
		}
	}
	if !ok {
		return nil, nil
	}
	object := store.GetObject(repo, oid)
	if object == nil {
		return nil, nil
	}
	if object.Type == "commit" && path != "" {
		tree := commitTreeOf(store, repo, oid)
		entries := map[string]string{}
		flattenTreeSimple(store, repo, tree, "", entries)
		hit, found := entries[path]
		if !found {
			return nil, nil
		}
		return gnode("Blob", map[string]any{"oid": hit, "repo": repo}), nil
	}
	switch object.Type {
	case "commit":
		return gnode("Commit", map[string]any{"oid": oid, "repo": repo}), nil
	case "tree":
		return gnode("Tree", map[string]any{"oid": oid, "repo": repo}), nil
	case "blob":
		return gnode("Blob", map[string]any{"oid": oid, "repo": repo}), nil
	}
	return gnode("Commit", map[string]any{"oid": oid, "repo": repo}), nil
}

// ---------------------------------------------------------------- global ids

func decodeGlobalID(store *Store, id string) (any, error) {
	raw := id
	if decoded, err := base64.StdEncoding.DecodeString(id); err == nil && len(decoded) > 0 {
		raw = string(decoded)
	}
	text := strings.NewReplacer("<", "", ">", "").Replace(raw)
	i := 0
	for i < len(text) && ((text[i] >= 'A' && text[i] <= 'Z') || (text[i] >= 'a' && text[i] <= 'z')) {
		i++
	}
	kind := text[:i]
	numberText := text[i:]
	if kind == "" || numberText == "" {
		return nil, fmt.Errorf("Could not resolve to a node with the global id of '%s'.", id)
	}
	number, err := strconv.Atoi(numberText)
	if err != nil {
		return nil, fmt.Errorf("Could not resolve to a node with the global id of '%s'.", id)
	}

	if kind == "Repository" {
		for _, repo := range store.Repos {
			if repo.ID == number || repo.NodeID == id {
				return gnode("Repository", repo), nil
			}
		}
		return nil, fmt.Errorf("Could not resolve to a node with the global id of '%s'.", id)
	}
	for _, repo := range store.Repos {
		if kind == "Issue" {
			for _, issue := range repo.Data.Issues {
				if issue.NodeID == id || issue.ID == number || issue.Number == number {
					return gnode("Issue", &issueNodePayload{Rec: issue, Repo: repo}), nil
				}
			}
		}
		if kind == "PullRequest" {
			for _, pull := range repo.Data.Pulls {
				if pull.NodeID == id || pull.ID == number || pull.Number == number {
					return gnode("PullRequest", &pullNodePayload{Rec: pull, Repo: repo}), nil
				}
			}
		}
	}
	if kind == "User" {
		for _, user := range store.Users {
			if user.NodeID == id || user.ID == number {
				return gnode("User", user), nil
			}
		}
	}
	return nil, fmt.Errorf("Could not resolve to a node with the global id of '%s'.", id)
}

func repoByID(ec *ExecCtx, id string) (*RepoRecord, error) {
	node, err := decodeGlobalID(ec.Store, id)
	if err != nil {
		return nil, fmt.Errorf("Could not resolve to a node with the global id of '%s'.", id)
	}
	gn, ok := isGNode(node)
	if !ok || gn.Gql != "Repository" {
		return nil, fmt.Errorf("Could not resolve to a node with the global id of '%s'.", id)
	}
	return gn.V.(*RepoRecord), nil
}

func issueByID(ec *ExecCtx, id string) (*issueNodePayload, error) {
	node, err := decodeGlobalID(ec.Store, id)
	if err != nil {
		return nil, fmt.Errorf("Could not resolve to an Issue with that id.")
	}
	gn, ok := isGNode(node)
	if !ok || gn.Gql != "Issue" {
		return nil, fmt.Errorf("Could not resolve to an Issue with that id.")
	}
	return gn.V.(*issueNodePayload), nil
}

func pullByID(ec *ExecCtx, id string) (*pullNodePayload, error) {
	node, err := decodeGlobalID(ec.Store, id)
	if err != nil {
		return nil, fmt.Errorf("Could not resolve to a PullRequest with that id.")
	}
	gn, ok := isGNode(node)
	if !ok || gn.Gql != "PullRequest" {
		return nil, fmt.Errorf("Could not resolve to a PullRequest with that id.")
	}
	return gn.V.(*pullNodePayload), nil
}

// ---------------------------------------------------------------- execution

func typenameOf(value any) string {
	if node, ok := isGNode(value); ok {
		return node.Gql
	}
	switch value.(type) {
	case *IntroType:
		return "__Type"
	case *IntroFieldRef:
		return "__Field"
	}
	if m, ok := value.(map[string]any); ok {
		if name, ok := m["__typename"].(string); ok {
			return name
		}
	}
	if _, ok := value.([]any); ok {
		return "Array"
	}
	return "Object"
}

func expandSelection(selection []Sel, value any, ec *ExecCtx) ([]*FieldNode, error) {
	out := []*FieldNode{}
	actual := typenameOf(value)
	for _, item := range selection {
		switch node := item.(type) {
		case *FieldNode:
			out = append(out, node)
		case *InlineNode:
			if node.TypeCond == "" || node.TypeCond == actual {
				children, err := expandSelection(node.Selection, value, ec)
				if err != nil {
					return nil, err
				}
				out = append(out, children...)
			}
		case *SpreadNode:
			fragment, ok := ec.Fragments[node.Name]
			if !ok {
				return nil, fmt.Errorf("Unknown fragment \"%s\".", node.Name)
			}
			if fragment.TypeCond == "" || fragment.TypeCond == actual {
				children, err := expandSelection(fragment.Selection, value, ec)
				if err != nil {
					return nil, err
				}
				out = append(out, children...)
			}
		}
	}
	return out, nil
}

func project(value any, selection []Sel, ec *ExecCtx, path []any) any {
	if value == nil {
		return nil
	}
	if list, ok := value.([]any); ok {
		out := make([]any, 0, len(list))
		for index, item := range list {
			out = append(out, project(item, selection, ec, append(append([]any{}, path...), index)))
		}
		return out
	}

	fields, err := expandSelection(selection, value, ec)
	if err != nil {
		ec.Errors = append(ec.Errors, &GqlError{Message: err.Error(), Path: append([]any{}, path...)})
		return nil
	}

	out := map[string]any{}
	for _, field := range fields {
		key := field.Name
		if field.HasAlias {
			key = field.Alias
		}
		if field.Name == "__typename" {
			out[key] = typenameOf(value)
			continue
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					ec.Errors = append(ec.Errors, &GqlError{
						Message:   errString(rec),
						Locations: []map[string]int{{"line": field.Line, "column": field.Col}},
						Path:      append(append([]any{}, path...), key),
					})
					out[key] = nil
				}
			}()
			args, argErr := evalArgs(field.Args, ec)
			if argErr != nil {
				ec.Errors = append(ec.Errors, &GqlError{
					Message:   argErr.Error(),
					Locations: []map[string]int{{"line": field.Line, "column": field.Col}},
					Path:      append(append([]any{}, path...), key),
				})
				out[key] = nil
				return
			}
			var resolved any
			typedName := ""
			var typedPayload any
			switch concrete := value.(type) {
			case *GNode:
				typedName, typedPayload = concrete.Gql, concrete.V
			case *IntroType:
				typedName, typedPayload = "__Type", concrete
			case *IntroFieldRef:
				typedName, typedPayload = "__Field", concrete
			}
			if typedName != "" {
				table, hasTable := resolvers[typedName]
				var resolver Resolver
				var hasResolver bool
				if hasTable {
					resolver, hasResolver = table[field.Name]
				}
				if !hasResolver {
					ec.Errors = append(ec.Errors, &GqlError{
						Message:   fmt.Sprintf("Cannot query field \"%s\" on type \"%s\".", field.Name, typedName),
						Locations: []map[string]int{{"line": field.Line, "column": field.Col}},
						Path:      append(append([]any{}, path...), key),
					})
					out[key] = nil
					return
				}
				value2, err := resolver(typedPayload, args, ec)
				if err != nil {
					ec.Errors = append(ec.Errors, &GqlError{
						Message:   err.Error(),
						Locations: []map[string]int{{"line": field.Line, "column": field.Col}},
						Path:      append(append([]any{}, path...), key),
					})
					out[key] = nil
					return
				}
				resolved = value2
			} else if m, ok := value.(map[string]any); ok {
				item, present := m[field.Name]
				if !present {
					ec.Errors = append(ec.Errors, &GqlError{
						Message:   fmt.Sprintf("Cannot query field \"%s\" on type \"%s\".", field.Name, typenameOf(value)),
						Locations: []map[string]int{{"line": field.Line, "column": field.Col}},
						Path:      append(append([]any{}, path...), key),
					})
					out[key] = nil
					return
				}
				resolved = item
			} else {
				ec.Errors = append(ec.Errors, &GqlError{
					Message:   fmt.Sprintf("Cannot query field \"%s\" on type \"%s\".", field.Name, typenameOf(value)),
					Locations: []map[string]int{{"line": field.Line, "column": field.Col}},
					Path:      append(append([]any{}, path...), key),
				})
				out[key] = nil
				return
			}
			if len(field.Selection) > 0 {
				out[key] = project(resolved, field.Selection, ec, append(append([]any{}, path...), key))
			} else if _, ok := isGNode(resolved); ok {
				out[key] = nil
			} else {
				out[key] = resolved
			}
		}()
	}
	return out
}

// ---------------------------------------------------------------- the request

func pickOperation(operations []*Operation, operationName string, hasName bool) (*Operation, string) {
	if len(operations) == 0 {
		return nil, "Must provide an operation."
	}
	if !hasName {
		if len(operations) > 1 {
			return nil, "Must provide operation name if query contains multiple operations."
		}
		return operations[0], ""
	}
	for _, operation := range operations {
		if operation.HasName && operation.Name == operationName {
			return operation, ""
		}
	}
	return nil, fmt.Sprintf("Unknown operation named \"%s\".", operationName)
}

func graphqlError(message string) map[string]any {
	return map[string]any{"errors": []any{map[string]any{"message": message}}}
}

func handleGraphql(ctx *Ctx) Reply {
	if !ctx.HasLogin || ctx.Login == "" {
		return R(401, map[string]any{"message": "Requires authentication"})
	}
	body := ctx.Body

	if list, ok := body.([]any); ok {
		if len(list) == 0 {
			return R(200, []any{})
		}
		replies := make([]any, 0, len(list))
		for _, entry := range list {
			nested := *ctx
			nested.Body = entry
			reply := handleGraphql(&nested)
			replies = append(replies, reply.Body)
		}
		return R(200, replies)
	}
	object, ok := body.(map[string]any)
	if !ok {
		return R(200, graphqlError("Problems parsing JSON"))
	}
	source, _ := asString(object["query"])
	if strings.TrimSpace(source) == "" {
		return R(200, graphqlError("Must provide query string."))
	}
	vars := map[string]any{}
	if m, ok := object["variables"].(map[string]any); ok {
		vars = m
	}
	operationName := ""
	hasOperationName := false
	if name, ok := asString(object["operationName"]); ok {
		operationName = name
		hasOperationName = true
	}

	operations, fragments, err := parseDocument(source)
	if err != nil {
		return R(200, graphqlError(err.Error()))
	}
	operation, pickErr := pickOperation(operations, operationName, hasOperationName)
	if operation == nil {
		return R(200, graphqlError(pickErr))
	}
	if operation.Type == "subscription" {
		return R(200, graphqlError("Subscriptions are not served by this forge."))
	}

	merged := map[string]any{}
	for key, value := range vars {
		merged[key] = value
	}
	ec := &ExecCtx{
		Store:     ctx.Store,
		Login:     ctx.Login,
		User:      ctx.User,
		Vars:      merged,
		Fragments: fragments,
	}
	for name, val := range operation.Defaults {
		if _, present := vars[name]; !present {
			evaluated, err := evalValue(val, ec)
			if err != nil {
				return R(200, graphqlError(err.Error()))
			}
			ec.Vars[name] = evaluated
		}
	}

	rootType := "Query"
	if operation.Type == "mutation" {
		rootType = "Mutation"
	}
	data := project(gnode(rootType, map[string]any{}), operation.Selection, ec, []any{})
	if len(ec.Errors) == 0 {
		return R(200, map[string]any{"data": data})
	}
	errors := make([]any, 0, len(ec.Errors))
	for _, item := range ec.Errors {
		entry := map[string]any{"message": item.Message}
		if len(item.Locations) > 0 {
			entry["locations"] = item.Locations
		}
		if len(item.Path) > 0 {
			entry["path"] = item.Path
		}
		errors = append(errors, entry)
	}
	return R(200, map[string]any{"data": data, "errors": errors})
}

func registerGraphql() {
	route("POST", "/api/graphql", func(ctx *Ctx) Reply {
		return handleGraphql(ctx)
	})
}

var _ = sort.Strings
var _ = strings.TrimSpace
