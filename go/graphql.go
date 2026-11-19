// graphql.go — GitHub GraphQL v4 for the documents gh and github-mcp-server
// actually send. A small parser for the GraphQL subset those documents use
// plus resolvers over the shared store. Ported from the TypeScript forge
// (graphql.ts), keeping the wire behaviour identical.
//
// JSON output order matters for GraphQL (fields answer in selection order), so
// execution builds ordered objects (ojObj) instead of Go maps.

package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// ordered JSON values
// ---------------------------------------------------------------------------

// ojVal is an ordered JSON value: null, bool, string, number, array, or an
// object whose fields keep insertion order.
type ojVal struct {
	kind   string // "null","bool","string","number","array","object"
	b      bool
	s      string
	n      float64
	arr    []*ojVal
	fields []ojField
}

type ojField struct {
	key string
	val *ojVal
}

func ojNull() *ojVal                     { return &ojVal{kind: "null"} }
func ojBool(b bool) *ojVal               { return &ojVal{kind: "bool", b: b} }
func ojString(s string) *ojVal           { return &ojVal{kind: "string", s: s} }
func ojInt(n int) *ojVal                 { return &ojVal{kind: "number", n: float64(n)} }
func ojNum(n float64) *ojVal             { return &ojVal{kind: "number", n: n} }
func ojArr(items ...*ojVal) *ojVal       { return &ojVal{kind: "array", arr: items} }
func ojObj(fields ...ojField) *ojVal     { return &ojVal{kind: "object", fields: fields} }
func (o *ojVal) set(key string, v *ojVal) *ojVal {
	o.fields = append(o.fields, ojField{key, v})
	return o
}
func (o *ojVal) get(key string) *ojVal {
	for _, f := range o.fields {
		if f.key == key {
			return f.val
		}
	}
	return nil
}

func jsonEscapeString(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case '\t':
			sb.WriteString("\\t")
		case '\b':
			sb.WriteString("\\b")
		case '\f':
			sb.WriteString("\\f")
		default:
			if r < 0x20 {
				fmt.Fprintf(sb, "\\u%04x", r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
}

// MarshalJSON renders the ordered value (Node JSON.stringify semantics:
// no HTML escaping, no spaces).
func (o *ojVal) MarshalJSON() ([]byte, error) {
	var sb strings.Builder
	o.writeJSON(&sb)
	return []byte(sb.String()), nil
}

func (o *ojVal) writeJSON(sb *strings.Builder) {
	switch o.kind {
	case "null":
		sb.WriteString("null")
	case "bool":
		if o.b {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case "string":
		jsonEscapeString(sb, o.s)
	case "number":
		if o.n == float64(int64(o.n)) && o.n < 1e15 && o.n > -1e15 {
			sb.WriteString(strconv.FormatInt(int64(o.n), 10))
		} else {
			sb.WriteString(strconv.FormatFloat(o.n, 'g', -1, 64))
		}
	case "array":
		sb.WriteByte('[')
		for i, item := range o.arr {
			if i > 0 {
				sb.WriteByte(',')
			}
			item.writeJSON(sb)
		}
		sb.WriteByte(']')
	case "object":
		sb.WriteByte('{')
		for i, f := range o.fields {
			if i > 0 {
				sb.WriteByte(',')
			}
			jsonEscapeString(sb, f.key)
			sb.WriteByte(':')
			f.val.writeJSON(sb)
		}
		sb.WriteByte('}')
	}
}

// toGo converts an ordered value to a plain Go value (for the HTTP layer,
// which re-encodes with encoding/json — the bytes are identical).
func (o *ojVal) toGo() any {
	switch o.kind {
	case "null":
		return nil
	case "bool":
		return o.b
	case "string":
		return o.s
	case "number":
		return o.n
	case "array":
		out := make([]any, len(o.arr))
		for i, item := range o.arr {
			out[i] = item.toGo()
		}
		return out
	case "object":
		out := &orderedMap{fields: o.fields}
		return out
	}
	return nil
}

// orderedMap is a map-like value that encoding/json renders in insertion order.
type orderedMap struct {
	fields []ojField
}

func (m *orderedMap) MarshalJSON() ([]byte, error) {
	v := &ojVal{kind: "object", fields: m.fields}
	return v.MarshalJSON()
}

// ---------------------------------------------------------------------------
// tokenizer + parser
// ---------------------------------------------------------------------------

type gqlToken struct {
	kind  string // "name","string","int","var","punct"
	value string
	num   int
}

func gqlTokenize(input string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(input) {
		ch := input[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(input) && input[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(input) && input[j] != '"' {
				if input[j] == '\\' && j+1 < len(input) {
					esc := input[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(input[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: "string", value: out.String()})
			i = j + 1
			continue
		}
		if ch == '$' {
			j := i + 1
			for j < len(input) && (isAlnumUnderscore(input[j])) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: "var", value: input[i+1 : j]})
			i = j
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			j := i
			if input[j] == '-' {
				j++
			}
			for j < len(input) && input[j] >= '0' && input[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(input[i:j])
			tokens = append(tokens, gqlToken{kind: "int", value: input[i:j], num: n})
			i = j
			continue
		}
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '_' {
			j := i
			for j < len(input) && isAlnumUnderscore(input[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: "name", value: input[i:j]})
			i = j
			continue
		}
		if strings.ContainsRune("{}()[]:!@=&|", rune(ch)) || ch == '.' {
			if ch == '.' && input[i:] != "" && len(input) >= i+3 && input[i:i+3] == "..." {
				tokens = append(tokens, gqlToken{kind: "punct", value: "..."})
				i += 3
				continue
			}
			tokens = append(tokens, gqlToken{kind: "punct", value: string(ch)})
			i++
			continue
		}
		return nil, fmt.Errorf("unexpected character %q in GraphQL document", string(ch))
	}
	return tokens, nil
}

func isAlnumUnderscore(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
}

type gqlValue struct {
	kind   string // "string","int","bool","null","enum","var","list","object"
	s      string
	n      int
	b      bool
	name   string
	items  []*gqlValue
	fields map[string]*gqlValue
}

type gqlField struct {
	alias      string
	hasAlias   bool
	name       string
	args       map[string]*gqlValue
	selections []gqlSelection // nil when none
}

type gqlSelection struct {
	kind          string // "field","inline","spread"
	field         *gqlField
	typeCondition string
	hasCondition  bool
	selections    []gqlSelection
	name          string
}

type gqlOperation struct {
	typ        string // "query" | "mutation"
	name       string
	selections []gqlSelection
}

type gqlDocument struct {
	operations []gqlOperation
	fragments  map[string][]gqlSelection
}

type gqlParser struct {
	pos    int
	tokens []gqlToken
}

func (p *gqlParser) peek() *gqlToken {
	if p.pos >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos]
}

func (p *gqlParser) next() (gqlToken, error) {
	if p.pos >= len(p.tokens) {
		return gqlToken{}, fmt.Errorf("unexpected end of GraphQL document")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(value string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != "punct" || t.value != value {
		return fmt.Errorf("expected %q in GraphQL document, got %+v", value, t)
	}
	return nil
}

func (p *gqlParser) expectName(value ...string) (string, error) {
	t, err := p.next()
	if err != nil {
		return "", err
	}
	if t.kind != "name" {
		return "", fmt.Errorf("expected a name in GraphQL document, got %+v", t)
	}
	if len(value) > 0 && t.value != value[0] {
		return "", fmt.Errorf("expected %s, got %s", value[0], t.value)
	}
	return t.value, nil
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: make(map[string][]gqlSelection)}
	for p.pos < len(p.tokens) {
		t := p.peek()
		if t.kind == "punct" && t.value == "{" {
			sels, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, gqlOperation{typ: "query", selections: sels})
			continue
		}
		if t.kind == "name" && (t.value == "query" || t.value == "mutation") {
			typTok, _ := p.next()
			typ := typTok.value
			if nxt := p.peek(); nxt != nil && nxt.kind == "name" {
				if _, err := p.next(); err != nil {
					return nil, err
				}
			}
			if nxt := p.peek(); nxt != nil && nxt.kind == "punct" && nxt.value == "(" {
				if err := p.skipParens(); err != nil {
					return nil, err
				}
			}
			sels, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, gqlOperation{typ: typ, selections: sels})
			continue
		}
		if t.kind == "name" && t.value == "fragment" {
			if _, err := p.next(); err != nil {
				return nil, err
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if _, err := p.expectName("on"); err != nil {
				return nil, err
			}
			if _, err := p.expectName(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelections()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = sels
			continue
		}
		return nil, fmt.Errorf("unexpected token %+v in GraphQL document", *t)
	}
	return doc, nil
}

func (p *gqlParser) skipParens() error {
	if err := p.expectPunct("("); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		t, err := p.next()
		if err != nil {
			return err
		}
		if t.kind == "punct" && t.value == "(" {
			depth++
		} else if t.kind == "punct" && t.value == ")" {
			depth--
		}
	}
	return nil
}

func (p *gqlParser) parseSelections() ([]gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []gqlSelection
	for {
		t := p.peek()
		if t == nil {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if t.kind == "punct" && t.value == "}" {
			p.pos++
			return sels, nil
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	t := p.peek()
	if t != nil && t.kind == "punct" && t.value == "..." {
		p.pos++
		nxt := p.peek()
		if nxt != nil && nxt.kind == "name" && nxt.value == "on" {
			p.pos++
			tc, err := p.expectName()
			if err != nil {
				return gqlSelection{}, err
			}
			sels, err := p.parseSelections()
			if err != nil {
				return gqlSelection{}, err
			}
			return gqlSelection{kind: "inline", typeCondition: tc, hasCondition: true, selections: sels}, nil
		}
		if nxt != nil && nxt.kind == "name" {
			nameTok, _ := p.next()
			return gqlSelection{kind: "spread", name: nameTok.value}, nil
		}
		sels, err := p.parseSelections()
		if err != nil {
			return gqlSelection{}, err
		}
		return gqlSelection{kind: "inline", selections: sels}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return gqlSelection{}, err
	}
	alias := ""
	hasAlias := false
	name := first
	if nxt := p.peek(); nxt != nil && nxt.kind == "punct" && nxt.value == ":" {
		p.pos++
		alias = first
		hasAlias = true
		name, err = p.expectName()
		if err != nil {
			return gqlSelection{}, err
		}
	}
	args := make(map[string]*gqlValue)
	if nxt := p.peek(); nxt != nil && nxt.kind == "punct" && nxt.value == "(" {
		p.pos++
		for {
			t := p.peek()
			if t == nil {
				return gqlSelection{}, fmt.Errorf("unterminated argument list")
			}
			if t.kind == "punct" && t.value == ")" {
				p.pos++
				break
			}
			argName, err := p.expectName()
			if err != nil {
				return gqlSelection{}, err
			}
			if err := p.expectPunct(":"); err != nil {
				return gqlSelection{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlSelection{}, err
			}
			args[argName] = v
		}
	}
	var sels []gqlSelection
	if nxt := p.peek(); nxt != nil && nxt.kind == "punct" && nxt.value == "{" {
		sels, err = p.parseSelections()
		if err != nil {
			return gqlSelection{}, err
		}
	}
	return gqlSelection{kind: "field", field: &gqlField{alias: alias, hasAlias: hasAlias, name: name, args: args, selections: sels}}, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	switch t.kind {
	case "string":
		return &gqlValue{kind: "string", s: t.value}, nil
	case "int":
		return &gqlValue{kind: "int", n: t.num}, nil
	case "var":
		return &gqlValue{kind: "var", name: t.value}, nil
	case "name":
		switch t.value {
		case "true":
			return &gqlValue{kind: "bool", b: true}, nil
		case "false":
			return &gqlValue{kind: "bool", b: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", s: t.value}, nil
		}
	case "punct":
		if t.value == "[" {
			var items []*gqlValue
			for {
				nxt := p.peek()
				if nxt == nil {
					return nil, fmt.Errorf("unterminated list")
				}
				if nxt.kind == "punct" && nxt.value == "]" {
					p.pos++
					return &gqlValue{kind: "list", items: items}, nil
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
		}
		if t.value == "{" {
			fields := make(map[string]*gqlValue)
			for {
				nxt := p.peek()
				if nxt == nil {
					return nil, fmt.Errorf("unterminated object")
				}
				if nxt.kind == "punct" && nxt.value == "}" {
					p.pos++
					return &gqlValue{kind: "object", fields: fields}, nil
				}
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				fields[name] = v
			}
		}
		return nil, fmt.Errorf("unexpected punctuation %s as a GraphQL value", t.value)
	}
	return nil, fmt.Errorf("unexpected token as a GraphQL value")
}

func parseGqlDocument(query string) (*gqlDocument, error) {
	tokens, err := gqlTokenize(query)
	if err != nil {
		return nil, err
	}
	return (&gqlParser{tokens: tokens}).parseDocument()
}

// ---------------------------------------------------------------------------
// value resolution
// ---------------------------------------------------------------------------

type gqlVariables map[string]any

func resolveGqlValue(v *gqlValue, vars gqlVariables) any {
	switch v.kind {
	case "string":
		return v.s
	case "int":
		return v.n
	case "bool":
		return v.b
	case "null":
		return nil
	case "enum":
		return v.s
	case "var":
		return vars[v.name]
	case "list":
		out := make([]any, len(v.items))
		for i, item := range v.items {
			out[i] = resolveGqlValue(item, vars)
		}
		return out
	case "object":
		out := make(map[string]any, len(v.fields))
		for k, child := range v.fields {
			out[k] = resolveGqlValue(child, vars)
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// schema field lists (for __type introspection)
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "body", "state", "author", "createdAt", "updatedAt", "closedAt", "labels", "comments", "url", "repository"},
	"PullRequest": {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "files", "author", "createdAt", "updatedAt", "url", "repository"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "createdAt", "updatedAt", "url"},
	"User":        {"id", "login", "name", "email"},
	"SearchType":  {},
}

var gqlSearchTypeValues = []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "MILESTONE"}

// ---------------------------------------------------------------------------
// resolved values
// ---------------------------------------------------------------------------

type gqlObject struct {
	typename string
	fields   map[string]any // values: string/int/bool/nil/*gqlObject/[]*gqlObject, or __repo/__issue/__pull
}

func gqlObj(typename string, fields map[string]any) *gqlObject {
	return &gqlObject{typename: typename, fields: fields}
}

func gqlUserObject() *gqlObject {
	return gqlObj("User", map[string]any{
		"id":    base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("User:%d", VIEWER.ID))),
		"login": VIEWER.Login, "name": VIEWER.Name, "email": VIEWER.Email,
	})
}

func gqlRepoObject(store *Store, repo *Repo) *gqlObject {
	var defaultBranchRef *gqlObject
	if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		defaultBranchRef = gqlObj("Ref", map[string]any{"name": repo.DefaultBranch})
	}
	fields := map[string]any{
		"id": repoGlobalID(repo), "databaseId": repo.DatabaseID, "name": repo.Name,
		"nameWithOwner": repo.Owner + "/" + repo.Name, "owner": gqlUserObject(),
		"defaultBranchRef": defaultBranchRef, "viewerPermission": "ADMIN",
		"hasIssuesEnabled": true, "description": repo.Description,
		"createdAt": repo.CreatedAt, "updatedAt": repo.UpdatedAt,
		"url": "https://github.com/" + repo.Owner + "/" + repo.Name,
		"__repo": repo,
	}
	return gqlObj("Repository", fields)
}

func gqlIssueObject(store *Store, repo *Repo, issue *Issue) *gqlObject {
	state := "CLOSED"
	if issue.State == "open" {
		state = "OPEN"
	}
	return gqlObj("Issue", map[string]any{
		"id": issueGlobalID(repo, issue), "number": issue.Number,
		"title": issue.Title, "body": issue.Body, "state": state,
		"author": gqlUserObject(), "createdAt": issue.CreatedAt, "updatedAt": issue.UpdatedAt,
		"closedAt": issue.ClosedAt,
		"url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		"__repo": repo, "__issue": issue,
	})
}

func gqlPullObject(store *Store, repo *Repo, pull *Pull) *gqlObject {
	headTip := pull.HeadSHA
	if v, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		headTip = v
	}
	baseTip := pull.BaseSHA
	if v, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseTip = v
	}
	state := "CLOSED"
	if pull.Merged {
		state = "MERGED"
	} else if pull.State == "open" {
		state = "OPEN"
	}
	mergeable := "CONFLICTING"
	if store.Mergeable(repo, pull) {
		mergeable = "MERGEABLE"
	}
	return gqlObj("PullRequest", map[string]any{
		"id": pullGlobalID(repo, pull), "number": pull.Number,
		"title": pull.Title, "body": pull.Body, "state": state, "merged": pull.Merged,
		"headRefName": pull.HeadRef, "baseRefName": pull.BaseRef,
		"headRefOid": headTip, "baseRefOid": baseTip, "mergeable": mergeable,
		"author": gqlUserObject(), "createdAt": pull.CreatedAt, "updatedAt": pull.UpdatedAt,
		"url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
		"__repo": repo, "__pull": pull,
	})
}

// decodeGlobalID decodes a global id of the form base64("Type:rest").
func decodeGlobalID(id string) (typ, rest string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	text := string(raw)
	colon := strings.Index(text, ":")
	if colon < 0 {
		return "", "", false
	}
	return text[:colon], text[colon+1:], true
}

func cursorFor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", number)))
}

func numberAfterCursor(cursor string) (int, bool) {
	if cursor == "" {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

type gqlExecContext struct {
	store     *Store
	variables gqlVariables
	errors    []string
}

func gqlArgValue(field *gqlField, name string, ctx *gqlExecContext) any {
	raw, ok := field.args[name]
	if !ok {
		return nil
	}
	return resolveGqlValue(raw, ctx.variables)
}

func gqlValueToOJ(v any, ctx *gqlExecContext, doc *gqlDocument, selections []gqlSelection, typeName string) *ojVal {
	switch t := v.(type) {
	case nil:
		return ojNull()
	case string:
		return ojString(t)
	case bool:
		return ojBool(t)
	case int:
		return ojInt(t)
	case float64:
		return ojNum(t)
	case *gqlObject:
		if selections == nil {
			return ojObj()
		}
		return executeSelections(selections, t, t.typename, ctx, doc)
	case []*gqlObject:
		items := make([]*ojVal, len(t))
		for i, node := range t {
			if selections == nil {
				items[i] = ojObj()
			} else {
				items[i] = executeSelections(selections, node, node.typename, ctx, doc)
			}
		}
		return ojArr(items...)
	}
	return ojNull()
}

func executeSelections(selections []gqlSelection, parent *gqlObject, typeName string, ctx *gqlExecContext, doc *gqlDocument) *ojVal {
	out := ojObj()
	for _, sel := range selections {
		if sel.kind == "spread" {
			if frag, ok := doc.fragments[sel.name]; ok {
				merged := executeSelections(frag, parent, typeName, ctx, doc)
				for _, f := range merged.fields {
					out.set(f.key, f.val)
				}
			}
			continue
		}
		if sel.kind == "inline" {
			if !sel.hasCondition || sel.typeCondition == typeName {
				merged := executeSelections(sel.selections, parent, typeName, ctx, doc)
				for _, f := range merged.fields {
					out.set(f.key, f.val)
				}
			}
			continue
		}
		field := sel.field
		key := field.name
		if field.hasAlias {
			key = field.alias
		}
		value := resolveField(parent, typeName, field, ctx, doc)
		out.set(key, value)
	}
	return out
}

func resolveField(parent *gqlObject, typeName string, field *gqlField, ctx *gqlExecContext, doc *gqlDocument) *ojVal {
	store := ctx.store
	// Root fields.
	if parent == nil {
		if typeName == "Mutation" {
			return resolveMutation(field, ctx, doc)
		}
		if field.name == "viewer" {
			target := gqlUserObject()
			if field.selections == nil {
				return gqlObjectToOJ(target, nil, ctx, doc)
			}
			return executeSelections(field.selections, target, "User", ctx, doc)
		}
		if field.name == "repository" {
			owner, _ := gqlArgValue(field, "owner", ctx).(string)
			name, _ := gqlArgValue(field, "name", ctx).(string)
			repo := store.GetRepo(owner, name)
			if repo == nil {
				return ojNull()
			}
			target := gqlRepoObject(store, repo)
			if field.selections == nil {
				return gqlObjectToOJ(target, nil, ctx, doc)
			}
			return executeSelections(field.selections, target, "Repository", ctx, doc)
		}
		if field.name == "node" {
			id, _ := gqlArgValue(field, "id", ctx).(string)
			typ, rest, ok := decodeGlobalID(id)
			if !ok {
				return ojNull()
			}
			var target *gqlObject
			switch typ {
			case "Repository":
				db, _ := strconv.Atoi(rest)
				for _, repo := range store.Repos() {
					if repo.DatabaseID == db {
						target = gqlRepoObject(store, repo)
						break
					}
				}
			case "Issue":
				parts := strings.SplitN(rest, ":", 2)
				db, _ := strconv.Atoi(parts[0])
				num, _ := strconv.Atoi(parts[1])
				for _, repo := range store.Repos() {
					if repo.DatabaseID != db {
						continue
					}
					if issue := store.GetIssue(repo, num); issue != nil {
						target = gqlIssueObject(store, repo, issue)
					}
					break
				}
			case "PullRequest":
				parts := strings.SplitN(rest, ":", 2)
				db, _ := strconv.Atoi(parts[0])
				num, _ := strconv.Atoi(parts[1])
				for _, repo := range store.Repos() {
					if repo.DatabaseID != db {
						continue
					}
					if pull := store.GetPull(repo, num); pull != nil {
						target = gqlPullObject(store, repo, pull)
					}
					break
				}
			}
			if target == nil {
				return ojNull()
			}
			if field.selections == nil {
				return gqlObjectToOJ(target, nil, ctx, doc)
			}
			return executeSelections(field.selections, target, target.typename, ctx, doc)
		}
		if field.name == "__type" {
			name, _ := gqlArgValue(field, "name", ctx).(string)
			fieldObjs := make([]*gqlObject, 0, len(gqlTypeFields[name]))
			for _, fn := range gqlTypeFields[name] {
				fieldObjs = append(fieldObjs, gqlObj("__Field", map[string]any{"name": fn}))
			}
			var enumObjs []*gqlObject
			if name == "SearchType" {
				for _, v := range gqlSearchTypeValues {
					enumObjs = append(enumObjs, gqlObj("__EnumValue", map[string]any{"name": v}))
				}
			}
			typeObj := gqlObj("__Type", map[string]any{"name": name, "fields": fieldObjs, "enumValues": enumObjs})
			if field.selections == nil {
				return gqlObjectToOJ(typeObj, nil, ctx, doc)
			}
			return executeSelections(field.selections, typeObj, "__Type", ctx, doc)
		}
		ctx.errors = append(ctx.errors, fmt.Sprintf("Cannot query field %q on type \"Query\".", field.name))
		return ojNull()
	}

	// Repository fields.
	if typeName == "Repository" {
		repo := parent.fields["__repo"].(*Repo)
		if field.name == "issue" {
			number, _ := gqlArgValue(field, "number", ctx).(int)
			// JSON numbers arrive as float64 via variables; handle both.
			if f, ok := gqlArgValue(field, "number", ctx).(float64); ok {
				number = int(f)
			}
			issue := store.GetIssue(repo, number)
			if issue == nil {
				return ojNull()
			}
			target := gqlIssueObject(store, repo, issue)
			if field.selections == nil {
				return gqlObjectToOJ(target, nil, ctx, doc)
			}
			return executeSelections(field.selections, target, "Issue", ctx, doc)
		}
		if field.name == "issues" {
			first := -1
			if f, ok := gqlArgValue(field, "first", ctx).(float64); ok {
				first = int(f)
			} else if n, ok := gqlArgValue(field, "first", ctx).(int); ok {
				first = n
			}
			after, _ := gqlArgValue(field, "after", ctx).(string)
			statesRaw := gqlArgValue(field, "states", ctx)
			orderBy, _ := gqlArgValue(field, "orderBy", ctx).(map[string]any)
			issues := append([]*Issue(nil), repo.Issues...)
			if states, ok := statesRaw.([]any); ok && len(states) > 0 {
				wanted := make(map[string]bool)
				for _, st := range states {
					wanted[strings.ToLower(fmt.Sprint(st))] = true
				}
				filtered := issues[:0]
				for _, issue := range issues {
					if wanted[issue.State] {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			direction := -1
			if orderBy != nil && orderBy["direction"] == "ASC" {
				direction = 1
			}
			sort.SliceStable(issues, func(i, j int) bool {
				return (issues[i].Number-issues[j].Number)*direction < 0
			})
			if afterNumber, ok := numberAfterCursor(after); ok {
				filtered := issues[:0]
				for _, issue := range issues {
					if direction == 1 && issue.Number > afterNumber {
						filtered = append(filtered, issue)
					} else if direction == -1 && issue.Number < afterNumber {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			limit := len(issues)
			if first >= 0 {
				limit = first
			}
			page := issues
			if len(page) > limit {
				page = page[:limit]
			}
			hasNextPage := len(issues) > limit
			var endCursor *string
			if len(page) > 0 {
				c := cursorFor(page[len(page)-1].Number)
				endCursor = &c
			}
			nodes := make([]*gqlObject, len(page))
			for i, issue := range page {
				nodes[i] = gqlIssueObject(store, repo, issue)
			}
			pageInfo := gqlObj("PageInfo", map[string]any{"hasNextPage": hasNextPage, "endCursor": endCursor})
			conn := gqlObj("IssueConnection", map[string]any{"nodes": nodes, "pageInfo": pageInfo, "totalCount": len(issues)})
			if field.selections == nil {
				return gqlObjectToOJ(conn, nil, ctx, doc)
			}
			return executeSelections(field.selections, conn, "IssueConnection", ctx, doc)
		}
		if field.name == "pullRequests" {
			first := -1
			if f, ok := gqlArgValue(field, "first", ctx).(float64); ok {
				first = int(f)
			} else if n, ok := gqlArgValue(field, "first", ctx).(int); ok {
				first = n
			}
			pulls := append([]*Pull(nil), repo.Pulls...)
			sort.SliceStable(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
			limit := len(pulls)
			if first >= 0 {
				limit = first
			}
			page := pulls
			if len(page) > limit {
				page = page[:limit]
			}
			nodes := make([]*gqlObject, len(page))
			for i, pull := range page {
				nodes[i] = gqlPullObject(store, repo, pull)
			}
			pageInfo := gqlObj("PageInfo", map[string]any{"hasNextPage": len(pulls) > limit, "endCursor": nil})
			conn := gqlObj("PullRequestConnection", map[string]any{"nodes": nodes, "pageInfo": pageInfo, "totalCount": len(pulls)})
			if field.selections == nil {
				return gqlObjectToOJ(conn, nil, ctx, doc)
			}
			return executeSelections(field.selections, conn, "PullRequestConnection", ctx, doc)
		}
		if field.name == "pullRequest" {
			number := 0
			if f, ok := gqlArgValue(field, "number", ctx).(float64); ok {
				number = int(f)
			} else if n, ok := gqlArgValue(field, "number", ctx).(int); ok {
				number = n
			}
			pull := store.GetPull(repo, number)
			if pull == nil {
				return ojNull()
			}
			target := gqlPullObject(store, repo, pull)
			if field.selections == nil {
				return gqlObjectToOJ(target, nil, ctx, doc)
			}
			return executeSelections(field.selections, target, "PullRequest", ctx, doc)
		}
		if field.name == "owner" {
			target := gqlUserObject()
			if field.selections == nil {
				return gqlObjectToOJ(target, nil, ctx, doc)
			}
			return executeSelections(field.selections, target, "User", ctx, doc)
		}
		if field.name == "defaultBranchRef" {
			ref, _ := parent.fields["defaultBranchRef"].(*gqlObject)
			if ref == nil {
				return ojNull()
			}
			if field.selections == nil {
				return gqlObjectToOJ(ref, nil, ctx, doc)
			}
			return executeSelections(field.selections, ref, "Ref", ctx, doc)
		}
		if value, ok := parent.fields[field.name]; ok {
			return gqlScalarToOJ(value, field, ctx, doc)
		}
		ctx.errors = append(ctx.errors, fmt.Sprintf("Cannot query field %q on type \"Repository\".", field.name))
		return ojNull()
	}

	// Connection types.
	if typeName == "IssueConnection" || typeName == "PullRequestConnection" || typeName == "PullRequestFileConnection" {
		if field.name == "nodes" {
			nodes, _ := parent.fields["nodes"].([]*gqlObject)
			childType := "Issue"
			if typeName == "PullRequestConnection" {
				childType = "PullRequest"
			} else if typeName == "PullRequestFileConnection" {
				childType = "PullRequestFile"
			}
			items := make([]*ojVal, len(nodes))
			for i, node := range nodes {
				if field.selections == nil {
					items[i] = gqlObjectToOJ(node, nil, ctx, doc)
				} else {
					items[i] = executeSelections(field.selections, node, childType, ctx, doc)
				}
			}
			return ojArr(items...)
		}
		if field.name == "pageInfo" {
			pageInfo, _ := parent.fields["pageInfo"].(*gqlObject)
			if field.selections == nil {
				return gqlObjectToOJ(pageInfo, nil, ctx, doc)
			}
			return executeSelections(field.selections, pageInfo, "PageInfo", ctx, doc)
		}
		if value, ok := parent.fields[field.name]; ok {
			return gqlScalarToOJ(value, field, ctx, doc)
		}
		ctx.errors = append(ctx.errors, fmt.Sprintf("Cannot query field %q on type %q.", field.name, typeName))
		return ojNull()
	}

	// PullRequest fields.
	if typeName == "PullRequest" {
		if field.name == "files" {
			first := -1
			if f, ok := gqlArgValue(field, "first", ctx).(float64); ok {
				first = int(f)
			} else if n, ok := gqlArgValue(field, "first", ctx).(int); ok {
				first = n
			}
			pull := parent.fields["__pull"].(*Pull)
			repo := parent.fields["__repo"].(*Repo)
			files := store.PullFiles(repo, pull)
			limit := len(files)
			if first >= 0 {
				limit = first
			}
			page := files
			if len(page) > limit {
				page = page[:limit]
			}
			nodes := make([]*gqlObject, len(page))
			for i, f := range page {
				nodes[i] = gqlObj("PullRequestFile", map[string]any{"path": f.Filename, "additions": f.Additions, "deletions": f.Deletions, "changeType": f.Status})
			}
			pageInfo := gqlObj("PageInfo", map[string]any{"hasNextPage": len(files) > limit, "endCursor": nil})
			conn := gqlObj("PullRequestFileConnection", map[string]any{"nodes": nodes, "pageInfo": pageInfo, "totalCount": len(files)})
			if field.selections == nil {
				return gqlObjectToOJ(conn, nil, ctx, doc)
			}
			return executeSelections(field.selections, conn, "PullRequestFileConnection", ctx, doc)
		}
		if value, ok := parent.fields[field.name]; ok {
			return gqlScalarToOJ(value, field, ctx, doc)
		}
		ctx.errors = append(ctx.errors, fmt.Sprintf("Cannot query field %q on type \"PullRequest\".", field.name))
		return ojNull()
	}

	// __Type introspection.
	if typeName == "__Type" {
		if field.name == "fields" {
			fields, _ := parent.fields["fields"].([]*gqlObject)
			items := make([]*ojVal, len(fields))
			for i, entry := range fields {
				if field.selections == nil {
					items[i] = gqlObjectToOJ(entry, nil, ctx, doc)
				} else {
					items[i] = executeSelections(field.selections, entry, "__Field", ctx, doc)
				}
			}
			return ojArr(items...)
		}
		if field.name == "enumValues" {
			values, _ := parent.fields["enumValues"].([]*gqlObject)
			items := make([]*ojVal, len(values))
			for i, entry := range values {
				if field.selections == nil {
					items[i] = gqlObjectToOJ(entry, nil, ctx, doc)
				} else {
					items[i] = executeSelections(field.selections, entry, "__EnumValue", ctx, doc)
				}
			}
			return ojArr(items...)
		}
		if value, ok := parent.fields[field.name]; ok {
			return gqlScalarToOJ(value, field, ctx, doc)
		}
		ctx.errors = append(ctx.errors, fmt.Sprintf("Cannot query field %q on type \"__Type\".", field.name))
		return ojNull()
	}

	// Plain object fields (User, Ref, PageInfo, __Field, __EnumValue, Issue,
	// PullRequestFile).
	if value, ok := parent.fields[field.name]; ok {
		if target, isObj := value.(*gqlObject); isObj && field.selections != nil {
			return executeSelections(field.selections, target, target.typename, ctx, doc)
		}
		return gqlScalarToOJ(value, field, ctx, doc)
	}
	ctx.errors = append(ctx.errors, fmt.Sprintf("Cannot query field %q on type %q.", field.name, typeName))
	return ojNull()
}

// gqlScalarToOJ converts a scalar field value to an ordered JSON value.
func gqlScalarToOJ(value any, field *gqlField, ctx *gqlExecContext, doc *gqlDocument) *ojVal {
	switch v := value.(type) {
	case nil:
		return ojNull()
	case string:
		return ojString(v)
	case bool:
		return ojBool(v)
	case int:
		return ojInt(v)
	case *string:
		if v == nil {
			return ojNull()
		}
		return ojString(*v)
	case *gqlObject:
		if field.selections == nil {
			return gqlObjectToOJ(v, nil, ctx, doc)
		}
		return executeSelections(field.selections, v, v.typename, ctx, doc)
	}
	return ojNull()
}

// gqlObjectToOJ renders a gqlObject with no selections as an empty object
// (the TS forge returns the object itself; JSON.stringify of it would include
// its fields — but a selection-less object field is never queried by the
// suite's documents; this path only matters for completeness).
func gqlObjectToOJ(obj *gqlObject, _ []gqlSelection, ctx *gqlExecContext, doc *gqlDocument) *ojVal {
	if obj == nil {
		return ojNull()
	}
	out := ojObj()
	for key, value := range obj.fields {
		if strings.HasPrefix(key, "__") {
			continue
		}
		out.set(key, gqlScalarToOJ(value, &gqlField{}, ctx, doc))
	}
	return out
}

func resolveMutation(field *gqlField, ctx *gqlExecContext, doc *gqlDocument) *ojVal {
	store := ctx.store
	input, _ := gqlArgValue(field, "input", ctx).(map[string]any)
	if field.name == "createIssue" {
		repositoryID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		typ, rest, ok := decodeGlobalID(repositoryID)
		var repo *Repo
		if ok && typ == "Repository" {
			db, _ := strconv.Atoi(rest)
			for _, candidate := range store.Repos() {
				if candidate.DatabaseID == db {
					repo = candidate
					break
				}
			}
		}
		if repo == nil || title == "" {
			ctx.errors = append(ctx.errors, "createIssue: repository not found")
			return ojNull()
		}
		body, _ := input["body"].(string)
		issue := store.CreateIssue(repo, title, body, nil)
		if issue.Error != "" {
			ctx.errors = append(ctx.errors, issue.Error)
			return ojNull()
		}
		payload := gqlObj("CreateIssuePayload", map[string]any{"issue": gqlIssueObject(store, repo, issue.Issue)})
		if field.selections == nil {
			return gqlObjectToOJ(payload, nil, ctx, doc)
		}
		return executeSelections(field.selections, payload, "CreateIssuePayload", ctx, doc)
	}
	if field.name == "closeIssue" {
		issueID, _ := input["issueId"].(string)
		typ, rest, ok := decodeGlobalID(issueID)
		var targetRepo *Repo
		var targetIssue *Issue
		if ok && typ == "Issue" {
			parts := strings.SplitN(rest, ":", 2)
			db, _ := strconv.Atoi(parts[0])
			num, _ := strconv.Atoi(parts[1])
			for _, repo := range store.Repos() {
				if repo.DatabaseID != db {
					continue
				}
				if issue := store.GetIssue(repo, num); issue != nil {
					targetRepo, targetIssue = repo, issue
				}
				break
			}
		}
		if targetRepo == nil || targetIssue == nil {
			ctx.errors = append(ctx.errors, "closeIssue: issue not found")
			return ojNull()
		}
		store.SetIssueState(targetRepo, targetIssue.Number, "closed")
		payload := gqlObj("CloseIssuePayload", map[string]any{"issue": gqlIssueObject(store, targetRepo, targetIssue)})
		if field.selections == nil {
			return gqlObjectToOJ(payload, nil, ctx, doc)
		}
		return executeSelections(field.selections, payload, "CloseIssuePayload", ctx, doc)
	}
	if field.name == "createPullRequest" {
		repositoryID, _ := input["repositoryId"].(string)
		typ, rest, ok := decodeGlobalID(repositoryID)
		var repo *Repo
		if ok && typ == "Repository" {
			db, _ := strconv.Atoi(rest)
			for _, candidate := range store.Repos() {
				if candidate.DatabaseID == db {
					repo = candidate
					break
				}
			}
		}
		headRefName, _ := input["headRefName"].(string)
		baseRefName, _ := input["baseRefName"].(string)
		title, _ := input["title"].(string)
		if repo == nil || headRefName == "" || baseRefName == "" || title == "" {
			ctx.errors = append(ctx.errors, "createPullRequest: repository or refs not found")
			return ojNull()
		}
		body, _ := input["body"].(string)
		pull := store.CreatePull(repo, title, body, headRefName, baseRefName)
		if pull.Error != "" {
			ctx.errors = append(ctx.errors, pull.Error)
			return ojNull()
		}
		payload := gqlObj("CreatePullRequestPayload", map[string]any{"pullRequest": gqlPullObject(store, repo, pull.Pull)})
		if field.selections == nil {
			return gqlObjectToOJ(payload, nil, ctx, doc)
		}
		return executeSelections(field.selections, payload, "CreatePullRequestPayload", ctx, doc)
	}
	if field.name == "mergePullRequest" {
		pullRequestID, _ := input["pullRequestId"].(string)
		typ, rest, ok := decodeGlobalID(pullRequestID)
		var targetRepo *Repo
		var targetPull *Pull
		if ok && typ == "PullRequest" {
			parts := strings.SplitN(rest, ":", 2)
			db, _ := strconv.Atoi(parts[0])
			num, _ := strconv.Atoi(parts[1])
			for _, repo := range store.Repos() {
				if repo.DatabaseID != db {
					continue
				}
				if pull := store.GetPull(repo, num); pull != nil {
					targetRepo, targetPull = repo, pull
				}
				break
			}
		}
		if targetRepo == nil || targetPull == nil {
			ctx.errors = append(ctx.errors, "mergePullRequest: pull request not found")
			return ojNull()
		}
		result := store.MergePull(targetRepo, targetPull)
		if result.Error != "" {
			ctx.errors = append(ctx.errors, result.Error)
			return ojNull()
		}
		payload := gqlObj("MergePullRequestPayload", map[string]any{"pullRequest": gqlPullObject(store, targetRepo, targetPull)})
		if field.selections == nil {
			return gqlObjectToOJ(payload, nil, ctx, doc)
		}
		return executeSelections(field.selections, payload, "MergePullRequestPayload", ctx, doc)
	}
	if field.name == "addComment" {
		subjectID, _ := input["subjectId"].(string)
		body, _ := input["body"].(string)
		typ, rest, ok := decodeGlobalID(subjectID)
		var targetRepo *Repo
		var targetIssue *Issue
		if ok && typ == "Issue" {
			parts := strings.SplitN(rest, ":", 2)
			db, _ := strconv.Atoi(parts[0])
			num, _ := strconv.Atoi(parts[1])
			for _, repo := range store.Repos() {
				if repo.DatabaseID != db {
					continue
				}
				if issue := store.GetIssue(repo, num); issue != nil {
					targetRepo, targetIssue = repo, issue
				}
				break
			}
		}
		if targetRepo == nil || targetIssue == nil || body == "" {
			// TS checks `!target || typeof body !== 'string'`; an empty string
			// body is still a string there. Preserve that: only fail when the
			// body is not a string (it always is from JSON) or no target.
			if targetRepo == nil || targetIssue == nil {
				ctx.errors = append(ctx.errors, "addComment: subject not found")
				return ojNull()
			}
		}
		comment := store.AddComment(targetRepo, targetIssue.Number, body)
		if comment == nil {
			ctx.errors = append(ctx.errors, "addComment: subject not found")
			return ojNull()
		}
		node := gqlObj("IssueComment", map[string]any{
			"id":   base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("IssueComment:%d:%d:%d", targetRepo.DatabaseID, targetIssue.Number, comment.ID))),
			"body": comment.Body,
		})
		payload := gqlObj("AddCommentPayload", map[string]any{"commentEdge": gqlObj("IssueCommentEdge", map[string]any{"node": node})})
		if field.selections == nil {
			return gqlObjectToOJ(payload, nil, ctx, doc)
		}
		return executeSelections(field.selections, payload, "AddCommentPayload", ctx, doc)
	}
	ctx.errors = append(ctx.errors, fmt.Sprintf("Cannot query field %q on type \"Mutation\".", field.name))
	return ojNull()
}

// ExecuteGraphQL executes one operation body; returns the GraphQL response object.
func ExecuteGraphQL(store *Store, query string, variables map[string]any) *ojVal {
	doc, err := parseGqlDocument(query)
	if err != nil {
		return ojObj().set("data", ojNull()).set("errors", ojArr(ojObj().set("message", ojString(err.Error()))))
	}
	if len(doc.operations) == 0 {
		return ojObj().set("data", ojNull()).set("errors", ojArr(ojObj().set("message", ojString("no operation in document"))))
	}
	operation := doc.operations[0]
	ctx := &gqlExecContext{store: store, variables: variables}
	rootType := "Query"
	if operation.typ == "mutation" {
		rootType = "Mutation"
	}
	data := executeSelections(operation.selections, nil, rootType, ctx, doc)
	resp := ojObj().set("data", data)
	if len(ctx.errors) > 0 {
		errs := make([]*ojVal, len(ctx.errors))
		for i, msg := range ctx.errors {
			errs[i] = ojObj().set("message", ojString(msg))
		}
		resp.set("errors", ojArr(errs...))
	}
	return resp
}

// HandleGraphQL handles POST /api/graphql: one operation or a JSON array batch.
func HandleGraphQL(store *Store, body any) (int, *ojVal) {
	if arr, ok := body.([]any); ok {
		responses := make([]*ojVal, len(arr))
		for i, entry := range arr {
			record, _ := entry.(map[string]any)
			query, _ := record["query"].(string)
			variables, _ := record["variables"].(map[string]any)
			responses[i] = ExecuteGraphQL(store, query, variables)
		}
		return 200, ojArr(responses...)
	}
	record, _ := body.(map[string]any)
	query, _ := record["query"].(string)
	variables, _ := record["variables"].(map[string]any)
	return 200, ExecuteGraphQL(store, query, variables)
}
