// mcp.go — Model Context Protocol over Streamable HTTP at /mcp (JSON-RPC 2.0).
// Speaks the modern revision (2026-07-28) via server/discover, and the legacy
// handshake (initialize + notifications/initialized) for older clients. The six
// required tools act on the same store as REST, so a repository or issue
// created over MCP reads back over /api/v3 as the same object. No Authorization
// header -> 401. A body that is not JSON -> HTTP 400 with JSON-RPC -32700. An
// unknown tool -> -32602. A call missing a required argument -> isError:true.
// Ported from the TypeScript forge (mcp.ts).

package main

import (
	"fmt"
	"math"
	"strings"
)

const mcpServerName = "github-forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// ---------------------------------------------------------------------------
// tool definitions (in the TS forge's order; key order preserved via ojVal)
// ---------------------------------------------------------------------------

type mcpProp struct {
	name string
	typ  string
	desc string
}

type mcpToolDef struct {
	name        string
	description string
	props       []mcpProp
	required    []string
}

func mcpToolDefs() []mcpToolDef {
	return []mcpToolDef{
		{
			name: "create_repo", description: "Create a new repository owned by the authenticated user.",
			props: []mcpProp{
				{"name", "string", "The repository name."},
				{"description", "string", "A short description of the repository."},
				{"private", "boolean", "Whether the repository is private."},
				{"owner", "string", "The owner login (defaults to the authenticated user)."},
			},
			required: []string{"name"},
		},
		{
			name: "get_repo", description: "Get a repository by owner and name.",
			props: []mcpProp{
				{"owner", "string", "The repository owner login."},
				{"repo", "string", "The repository name."},
			},
			required: []string{"owner", "repo"},
		},
		{
			name: "create_issue", description: "Create an issue in a repository.",
			props: []mcpProp{
				{"owner", "string", "The repository owner login."},
				{"repo", "string", "The repository name."},
				{"title", "string", "The issue title."},
				{"body", "string", "The issue body."},
			},
			required: []string{"owner", "repo", "title"},
		},
		{
			name: "list_issues", description: "List the issues of a repository.",
			props: []mcpProp{
				{"owner", "string", "The repository owner login."},
				{"repo", "string", "The repository name."},
				{"state", "string", "Filter by state: open, closed or all."},
			},
			required: []string{"owner", "repo"},
		},
		{
			name: "create_pull_request", description: "Create a pull request from a head branch into a base branch.",
			props: []mcpProp{
				{"owner", "string", "The repository owner login."},
				{"repo", "string", "The repository name."},
				{"title", "string", "The pull request title."},
				{"body", "string", "The pull request body."},
				{"head", "string", "The head branch name."},
				{"base", "string", "The base branch name."},
			},
			required: []string{"owner", "repo", "title", "head", "base"},
		},
		{
			name: "merge_pull_request", description: "Squash-merge a pull request.",
			props: []mcpProp{
				{"owner", "string", "The repository owner login."},
				{"repo", "string", "The repository name."},
				{"pull_number", "number", "The pull request number."},
			},
			required: []string{"owner", "repo", "pull_number"},
		},
	}
}

func mcpToolDefOJ(def mcpToolDef) *ojVal {
	props := ojObj()
	for _, p := range def.props {
		props.set(p.name, ojObj().set("type", ojString(p.typ)).set("description", ojString(p.desc)))
	}
	required := make([]*ojVal, len(def.required))
	for i, r := range def.required {
		required[i] = ojString(r)
	}
	return ojObj().
		set("name", ojString(def.name)).
		set("description", ojString(def.description)).
		set("inputSchema", ojObj().
			set("type", ojString("object")).
			set("properties", props).
			set("required", ojArr(required...)))
}

func mcpToolsListOJ() *ojVal {
	defs := mcpToolDefs()
	tools := make([]*ojVal, len(defs))
	for i, def := range defs {
		tools[i] = mcpToolDefOJ(def)
	}
	return ojObj().set("tools", ojArr(tools...))
}

// ---------------------------------------------------------------------------
// argument helpers
// ---------------------------------------------------------------------------

// mcpStr mirrors the TS str(): a non-empty string, else ok=false.
func mcpStr(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func mcpNum(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func mcpNormalize(name string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func mcpFindTool(name string) *mcpToolDef {
	normalized := mcpNormalize(name)
	defs := mcpToolDefs()
	for i := range defs {
		if mcpNormalize(defs[i].name) == normalized {
			return &defs[i]
		}
	}
	for i := range defs {
		if strings.HasSuffix(normalized, mcpNormalize(defs[i].name)) {
			return &defs[i]
		}
	}
	return nil
}

// splitFullName splits "owner/repo" into its parts.
func splitFullName(value string) (string, string, bool) {
	slash := strings.Index(value, "/")
	if slash <= 0 || slash == len(value)-1 {
		return "", "", false
	}
	return value[:slash], value[slash+1:], true
}

// resolveRepoArgs resolves owner/repo from tool arguments (owner+repo, or a full_name).
func resolveRepoArgs(args map[string]any) (string, string, bool) {
	owner, ownerOK := mcpStr(args["owner"])
	repo, repoOK := mcpStr(args["repo"])
	if !repoOK {
		repo, repoOK = mcpStr(args["repository"])
	}
	if ownerOK && repoOK {
		return owner, repo, true
	}
	for _, key := range []string{"full_name", "repo_full_name", "fullName"} {
		if fullName, ok := mcpStr(args[key]); ok {
			if o, r, ok := splitFullName(fullName); ok {
				return o, r, true
			}
		}
	}
	return "", "", false
}

// ---------------------------------------------------------------------------
// tool results
// ---------------------------------------------------------------------------

func mcpToolResult(text string, isError bool) *ojVal {
	out := ojObj().set("content", ojArr(ojObj().set("type", ojString("text")).set("text", ojString(text))))
	if isError {
		out.set("isError", ojBool(true))
	}
	return out
}

func mcpToolResultJSON(v any) *ojVal {
	raw := marshalJSON(v)
	// marshalJSON appends '\n'; JSON.stringify does not.
	text := strings.TrimSuffix(string(raw), "\n")
	return mcpToolResult(text, false)
}

type mcpOutcome struct {
	result   *ojVal // tool result object (nil when rpcErr is set)
	rpcErr   *mcpRPCErrorInfo
}

type mcpRPCErrorInfo struct {
	code    int
	message string
}

// mcpCallTool executes one tool call.
func mcpCallTool(store *Store, name string, args map[string]any) mcpOutcome {
	def := mcpFindTool(name)
	if def == nil {
		return mcpOutcome{rpcErr: &mcpRPCErrorInfo{code: -32602, message: "unknown tool: " + name}}
	}
	switch def.name {
	case "create_repo":
		repoName, ok := mcpStr(args["name"])
		if !ok {
			repoName, ok = mcpStr(args["repo"])
		}
		if !ok {
			repoName, ok = mcpStr(args["repository"])
		}
		if !ok {
			return mcpOutcome{result: mcpToolResult("create_repo requires a name", true)}
		}
		if store.GetRepo(VIEWER.Login, repoName) != nil {
			return mcpOutcome{result: mcpToolResult(fmt.Sprintf("Repository already exists: %s/%s", VIEWER.Login, repoName), true)}
		}
		private := false
		if b, ok := args["private"].(bool); ok {
			private = b
		}
		description, _ := mcpStr(args["description"])
		repo := store.CreateRepo(repoName, private, description)
		return mcpOutcome{result: mcpToolResultJSON(repoJSONOf(repo))}
	case "get_repo":
		owner, repoName, ok := resolveRepoArgs(args)
		if !ok {
			return mcpOutcome{result: mcpToolResult("get_repo requires owner and repo", true)}
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpOutcome{result: mcpToolResult(fmt.Sprintf("Not Found: %s/%s", owner, repoName), true)}
		}
		return mcpOutcome{result: mcpToolResultJSON(repoJSONOf(repo))}
	case "create_issue":
		owner, repoName, ok := resolveRepoArgs(args)
		title, titleOK := mcpStr(args["title"])
		if !ok || !titleOK {
			return mcpOutcome{result: mcpToolResult("create_issue requires owner, repo and title", true)}
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpOutcome{result: mcpToolResult(fmt.Sprintf("Not Found: %s/%s", owner, repoName), true)}
		}
		body, _ := mcpStr(args["body"])
		result := store.CreateIssue(repo, title, body, []string{})
		if result.Error != "" {
			return mcpOutcome{result: mcpToolResult(result.Error, true)}
		}
		return mcpOutcome{result: mcpToolResultJSON(issueJSONOf(repo, result.Issue))}
	case "list_issues":
		owner, repoName, ok := resolveRepoArgs(args)
		if !ok {
			return mcpOutcome{result: mcpToolResult("list_issues requires owner and repo", true)}
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpOutcome{result: mcpToolResult(fmt.Sprintf("Not Found: %s/%s", owner, repoName), true)}
		}
		state, _ := mcpStr(args["state"])
		issues := store.ListIssues(repo, state, "created", "desc")
		out := make([]issueJSON, 0, len(issues))
		for _, issue := range issues {
			out = append(out, issueJSONOf(repo, issue))
		}
		return mcpOutcome{result: mcpToolResultJSON(out)}
	case "create_pull_request":
		owner, repoName, ok := resolveRepoArgs(args)
		title, titleOK := mcpStr(args["title"])
		head, headOK := mcpStr(args["head"])
		if !headOK {
			head, headOK = mcpStr(args["head_ref"])
		}
		if !headOK {
			head, headOK = mcpStr(args["head_branch"])
		}
		base, baseOK := mcpStr(args["base"])
		if !baseOK {
			base, baseOK = mcpStr(args["base_ref"])
		}
		if !baseOK {
			base, baseOK = mcpStr(args["base_branch"])
		}
		if !ok || !titleOK || !headOK || !baseOK {
			return mcpOutcome{result: mcpToolResult("create_pull_request requires owner, repo, title, head and base", true)}
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpOutcome{result: mcpToolResult(fmt.Sprintf("Not Found: %s/%s", owner, repoName), true)}
		}
		body, _ := mcpStr(args["body"])
		result := store.CreatePull(repo, title, body, head, base)
		if result.Error != "" {
			return mcpOutcome{result: mcpToolResult(result.Error, true)}
		}
		return mcpOutcome{result: mcpToolResultJSON(pullJSONOf(repo, result.Pull))}
	case "merge_pull_request":
		owner, repoName, ok := resolveRepoArgs(args)
		var number float64
		numberOK := false
		for _, key := range []string{"pull_number", "pull_request_number", "number"} {
			if n, good := mcpNum(args[key]); good {
				number, numberOK = n, true
				break
			}
		}
		if !ok || !numberOK {
			return mcpOutcome{result: mcpToolResult("merge_pull_request requires owner, repo and pull_number", true)}
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpOutcome{result: mcpToolResult(fmt.Sprintf("Not Found: %s/%s", owner, repoName), true)}
		}
		pull := store.GetPull(repo, int(number))
		if pull == nil {
			return mcpOutcome{result: mcpToolResult(fmt.Sprintf("Not Found: pull request #%d", int(number)), true)}
		}
		result := store.MergePull(repo, pull)
		if result.Error != "" {
			return mcpOutcome{result: mcpToolResult(result.Error, true)}
		}
		return mcpOutcome{result: mcpToolResultJSON(pullJSONOf(repo, pull))}
	}
	return mcpOutcome{rpcErr: &mcpRPCErrorInfo{code: -32602, message: "unknown tool: " + name}}
}

// ---------------------------------------------------------------------------
// JSON-RPC handling
// ---------------------------------------------------------------------------

func mcpRPCResult(id any, result *ojVal) *ojVal {
	return ojObj().set("jsonrpc", ojString("2.0")).set("id", ojFromGo(id)).set("result", result)
}

func mcpRPCError(id any, code int, message string) *ojVal {
	return ojObj().set("jsonrpc", ojString("2.0")).set("id", ojFromGo(id)).
		set("error", ojObj().set("code", ojInt(code)).set("message", ojString(message)))
}

// ojFromGo converts a plain Go value to an ordered JSON value with
// deterministic key order (used for JSON-RPC ids and small envelopes).
func ojFromGo(v any) *ojVal {
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
	case []any:
		items := make([]*ojVal, len(t))
		for i, item := range t {
			items[i] = ojFromGo(item)
		}
		return ojArr(items...)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// Deterministic order: sort keys (envelope maps are tiny).
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[j] < keys[i] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		obj := ojObj()
		for _, k := range keys {
			obj.set(k, ojFromGo(t[k]))
		}
		return obj
	}
	return ojNull()
}

// HandleMCP handles POST /mcp. Returns (status, response); a nil response
// means an empty 202 (notification).
func HandleMCP(store *Store, body any) (int, *ojVal) {
	if body == nil {
		return 400, ojObj().set("jsonrpc", ojString("2.0")).set("id", ojNull()).
			set("error", ojObj().set("code", ojInt(-32600)).set("message", ojString("Invalid Request")))
	}
	record, ok := body.(map[string]any)
	if !ok {
		// Arrays and other non-objects are invalid requests.
		return 400, ojObj().set("jsonrpc", ojString("2.0")).set("id", ojNull()).
			set("error", ojObj().set("code", ojInt(-32600)).set("message", ojString("Invalid Request")))
	}
	method, _ := record["method"].(string)
	rawID, hasID := record["id"]
	id := rawID
	if !hasID || rawID == nil {
		// A notification (no id, or a null id) gets no JSON-RPC response.
		return 202, nil
	}
	params, _ := record["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	if method == "server/discover" {
		meta, _ := params["_meta"].(map[string]any)
		requested := "2026-07-28"
		if meta != nil {
			if v, ok := meta["io.modelcontextprotocol/protocolVersion"].(string); ok {
				requested = v
			} else if v, ok := params["protocolVersion"].(string); ok {
				requested = v
			}
		} else if v, ok := params["protocolVersion"].(string); ok {
			requested = v
		}
		supported := false
		for _, v := range mcpSupportedVersions {
			if v == requested {
				supported = true
				break
			}
		}
		if !supported {
			return 200, mcpRPCError(id, -32022, "Unsupported protocol version: "+requested)
		}
		versions := make([]*ojVal, len(mcpSupportedVersions))
		for i, v := range mcpSupportedVersions {
			versions[i] = ojString(v)
		}
		return 200, mcpRPCResult(id, ojObj().
			set("resultType", ojString("discover")).
			set("supportedVersions", ojArr(versions...)).
			set("capabilities", ojObj().set("tools", ojObj())).
			set("instructions", ojString("A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP, GraphQL and MCP tools.")).
			set("serverInfo", ojObj().set("name", ojString(mcpServerName)).set("version", ojString(mcpServerVersion))))
	}

	if method == "initialize" {
		requested, _ := params["protocolVersion"].(string)
		chosen := mcpSupportedVersions[0]
		for _, v := range mcpSupportedVersions {
			if v == requested {
				chosen = v
				break
			}
		}
		return 200, mcpRPCResult(id, ojObj().
			set("protocolVersion", ojString(chosen)).
			set("capabilities", ojObj().set("tools", ojObj())).
			set("serverInfo", ojObj().set("name", ojString(mcpServerName)).set("version", ojString(mcpServerVersion))))
	}

	if method == "tools/list" {
		return 200, mcpRPCResult(id, mcpToolsListOJ())
	}

	if method == "tools/call" {
		toolName, _ := params["name"].(string)
		if toolName == "" {
			return 200, mcpRPCError(id, -32602, "tools/call requires a tool name")
		}
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		outcome := mcpCallTool(store, toolName, args)
		if outcome.rpcErr != nil {
			return 200, mcpRPCError(id, outcome.rpcErr.code, outcome.rpcErr.message)
		}
		return 200, mcpRPCResult(id, outcome.result)
	}

	if method == "ping" {
		return 200, mcpRPCResult(id, ojObj())
	}

	return 200, mcpRPCError(id, -32601, "Method not found: "+method)
}
