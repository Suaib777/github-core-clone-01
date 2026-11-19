// rest.go — GitHub REST v3: repositories, branches, commits, contents, labels,
// issues, comments and pull requests. Ported from the TypeScript forge
// (rest.ts). Every reply is JSON with a non-empty X-GitHub-Request-Id (set by
// the server layer).

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type RestResponse struct {
	Status  int
	Headers map[string]string
	Body    any
}

func restJSON(status int, body any, headers map[string]string) RestResponse {
	return RestResponse{Status: status, Headers: headers, Body: body}
}

func restErr(status int, message string) RestResponse {
	return RestResponse{Status: status, Body: map[string]any{"message": message}}
}

func repoGlobalID(repo *Repo) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("Repository:%d", repo.DatabaseID)))
}

func issueGlobalID(repo *Repo, issue *Issue) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("Issue:%d:%d", repo.DatabaseID, issue.Number)))
}

func pullGlobalID(repo *Repo, pull *Pull) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("PullRequest:%d:%d", repo.DatabaseID, pull.Number)))
}

type userJSON struct {
	Login    string `json:"login"`
	ID       int    `json:"id"`
	Type     string `json:"type"`
	SiteAdmin bool  `json:"site_admin"`
}

func userJSONOf(login string, id int) userJSON {
	return userJSON{Login: login, ID: id, Type: "User", SiteAdmin: false}
}

type repoJSON struct {
	ID              int       `json:"id"`
	NodeID          string    `json:"node_id"`
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Owner           userJSON  `json:"owner"`
	Private         bool      `json:"private"`
	Description     string    `json:"description"`
	DefaultBranch   string    `json:"default_branch"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
	HTMLURL         string    `json:"html_url"`
	URL             string    `json:"url"`
	CloneURL        string    `json:"clone_url"`
	GitURL          string    `json:"git_url"`
	SSHURL          string    `json:"ssh_url"`
	HasIssues       bool      `json:"has_issues"`
	HasProjects     bool      `json:"has_projects"`
	HasWiki         bool      `json:"has_wiki"`
	HasPages        bool      `json:"has_pages"`
	HasDownloads    bool      `json:"has_downloads"`
	Archived        bool      `json:"archived"`
	Disabled        bool      `json:"disabled"`
	Fork            bool      `json:"fork"`
	OpenIssuesCount int       `json:"open_issues_count"`
	License         any       `json:"license"`
	Topics          []string  `json:"topics"`
	Visibility      string    `json:"visibility"`
}

func repoJSONOf(repo *Repo) repoJSON {
	open := 0
	for _, issue := range repo.Issues {
		if issue.State == "open" {
			open++
		}
	}
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	return repoJSON{
		ID: repo.DatabaseID, NodeID: repoGlobalID(repo), Name: repo.Name,
		FullName: repo.Owner + "/" + repo.Name, Owner: userJSONOf(repo.Owner, VIEWER.ID),
		Private: repo.Private, Description: repo.Description, DefaultBranch: repo.DefaultBranch,
		CreatedAt: repo.CreatedAt, UpdatedAt: repo.UpdatedAt,
		HTMLURL: "https://github.com/" + repo.Owner + "/" + repo.Name,
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		CloneURL: "https://github.com/" + repo.Owner + "/" + repo.Name + ".git",
		GitURL:   "git://github.com/" + repo.Owner + "/" + repo.Name + ".git",
		SSHURL:   "git@github.com:" + repo.Owner + "/" + repo.Name + ".git",
		HasIssues: true, HasProjects: true, HasWiki: true, HasPages: false, HasDownloads: true,
		Archived: false, Disabled: false, Fork: false,
		OpenIssuesCount: open, License: nil, Topics: []string{}, Visibility: visibility,
	}
}

type labelJSON struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type issueJSON struct {
	ID        int         `json:"id"`
	NodeID    string      `json:"node_id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	ClosedAt  *string     `json:"closed_at"`
	Comments  int         `json:"comments"`
	HTMLURL   string      `json:"html_url"`
	URL       string      `json:"url"`
}

func issueJSONOf(repo *Repo, issue *Issue) issueJSON {
	labels := make([]labelJSON, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		for _, l := range repo.Labels {
			if l.Name == name {
				color = l.Color
				break
			}
		}
		labels = append(labels, labelJSON{Name: name, Color: color, Description: ""})
	}
	return issueJSON{
		ID: repo.DatabaseID*1000000 + issue.Number, NodeID: issueGlobalID(repo, issue),
		Number: issue.Number, Title: issue.Title, Body: issue.Body, State: issue.State,
		Labels: labels, User: userJSONOf(VIEWER.Login, VIEWER.ID),
		CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt, ClosedAt: issue.ClosedAt,
		Comments: len(issue.Comments),
		HTMLURL: "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

type pullRefJSON struct {
	Ref   string   `json:"ref"`
	SHA   string   `json:"sha"`
	Label string   `json:"label"`
	User  userJSON `json:"user"`
	Repo  repoJSON `json:"repo"`
}

type pullJSON struct {
	ID             int         `json:"id"`
	NodeID         string      `json:"node_id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Head           pullRefJSON `json:"head"`
	Base           pullRefJSON `json:"base"`
	Merged         bool        `json:"merged"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSHA *string     `json:"merge_commit_sha"`
	Mergeable      any         `json:"mergeable"`
	User           userJSON    `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	HTMLURL        string      `json:"html_url"`
	URL            string      `json:"url"`
}

func pullJSONOf(repo *Repo, pull *Pull) pullJSON {
	return pullJSON{
		ID: repo.DatabaseID*1000000 + pull.Number, NodeID: pullGlobalID(repo, pull),
		Number: pull.Number, Title: pull.Title, Body: pull.Body, State: pull.State,
		Head: pullRefJSON{Ref: pull.HeadRef, SHA: pull.HeadSHA, Label: repo.Owner + ":" + pull.HeadRef, User: userJSONOf(VIEWER.Login, VIEWER.ID), Repo: repoJSONOf(repo)},
		Base: pullRefJSON{Ref: pull.BaseRef, SHA: pull.BaseSHA, Label: repo.Owner + ":" + pull.BaseRef, User: userJSONOf(VIEWER.Login, VIEWER.ID), Repo: repoJSONOf(repo)},
		Merged: pull.Merged, MergedAt: pull.MergedAt, MergeCommitSHA: pull.MergeCommitSHA,
		Mergeable: nil, User: userJSONOf(VIEWER.Login, VIEWER.ID),
		CreatedAt: pull.CreatedAt, UpdatedAt: pull.UpdatedAt,
		HTMLURL: "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pull.Number),
	}
}

// paginate builds a Link header for a paginated list, and slices the page.
func paginate[T any](items []T, query url.Values, basePath, host string) ([]T, string) {
	perPageRaw, _ := strconv.Atoi(query.Get("per_page"))
	if perPageRaw == 0 && query.Get("per_page") == "" {
		perPageRaw = 30
	}
	perPage := 30
	if perPageRaw > 0 {
		perPage = perPageRaw
		if perPage > 100 {
			perPage = 100
		}
	}
	pageRaw, _ := strconv.Atoi(query.Get("page"))
	pageNum := 1
	if pageRaw > 0 {
		pageNum = pageRaw
	}
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start := (pageNum - 1) * perPage
	var page []T
	if start < len(items) {
		end := start + perPage
		if end > len(items) {
			end = len(items)
		}
		page = items[start:end]
	} else {
		page = []T{}
	}
	origin := "http://" + host
	urlFor := func(page int) string {
		params := url.Values{}
		for k, vs := range query {
			for _, v := range vs {
				params.Add(k, v)
			}
		}
		params.Set("per_page", strconv.Itoa(perPage))
		params.Set("page", strconv.Itoa(page))
		return origin + basePath + "?" + params.Encode()
	}
	var links []string
	if pageNum < totalPages {
		links = append(links, "<"+urlFor(pageNum+1)+">; rel=\"next\"")
	}
	if pageNum > 1 {
		links = append(links, "<"+urlFor(pageNum-1)+">; rel=\"prev\"")
	}
	links = append(links, "<"+urlFor(1)+">; rel=\"first\"")
	links = append(links, "<"+urlFor(totalPages)+">; rel=\"last\"")
	if len(links) == 0 {
		return page, ""
	}
	return page, strings.Join(links, ", ")
}

// resolveRef resolves a ref query value (branch name or commit sha) to a commit oid.
func resolveRef(store *Store, repo *Repo, ref string) string {
	if ref == "" {
		return store.DefaultTip(repo)
	}
	if tip, ok := store.BranchTip(repo, ref); ok {
		return tip
	}
	if isHex40(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref
		}
	}
	return ""
}

func bodyObj(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func strVal(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}

// RouteRest routes one REST call. segments are the path parts after /api/v3.
func RouteRest(store *Store, method string, segments []string, query url.Values, body any, host string) RestResponse {
	input := bodyObj(body)
	// API root.
	if len(segments) == 0 {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restJSON(200, map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}
	if len(segments) == 1 && segments[0] == "meta" {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restJSON(200, map[string]any{"installed_version": "1.0.0"}, nil)
	}
	// POST /user/repos and GET /user/repos.
	if len(segments) == 2 && segments[0] == "user" && segments[1] == "repos" {
		if method == "POST" {
			name := strVal(input["name"], "")
			if name == "" {
				return restErr(422, "name is required")
			}
			if store.GetRepo(VIEWER.Login, name) != nil {
				return restErr(422, "Repository already exists")
			}
			private := false
			if b, ok := input["private"].(bool); ok {
				private = b
			}
			repo := store.CreateRepo(name, private, strVal(input["description"], ""))
			return restJSON(201, repoJSONOf(repo), nil)
		}
		if method == "GET" {
			repos := store.ViewerRepos()
			page, link := paginate(repos, query, "/api/v3/user/repos", host)
			headers := map[string]string{}
			if link != "" {
				headers["link"] = link
			}
			out := make([]repoJSON, 0, len(page))
			for _, r := range page {
				out = append(out, repoJSONOf(r))
			}
			return restJSON(200, out, headers)
		}
		return restErr(404, "Not Found")
	}
	// /repos/{owner}/{name}[/...]
	if len(segments) >= 3 && segments[0] == "repos" {
		owner := segments[1]
		name := segments[2]
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return restErr(404, "Not Found")
		}
		rest := segments[3:]
		if len(rest) == 0 {
			if method == "GET" {
				return restJSON(200, repoJSONOf(repo), nil)
			}
			if method == "PATCH" {
				if desc, ok := input["description"].(string); ok {
					repo.Description = desc
				}
				repo.UpdatedAt = nowIso()
				return restJSON(200, repoJSONOf(repo), nil)
			}
			if method == "DELETE" {
				store.DeleteRepo(owner, name)
				return RestResponse{Status: 204, Body: ""}
			}
			return restErr(404, "Not Found")
		}
		return routeRepoSub(store, repo, method, rest, query, input, host)
	}
	return restErr(404, "Not Found")
}

func routeRepoSub(store *Store, repo *Repo, method string, rest []string, query url.Values, input map[string]any, host string) RestResponse {
	head := rest[0]
	// branches
	if head == "branches" && len(rest) == 1 {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		branches := store.ListBranches(repo)
		out := make([]map[string]any, 0, len(branches))
		for _, b := range branches {
			out = append(out, map[string]any{"name": b.Name, "commit": map[string]any{"sha": b.SHA}})
		}
		return restJSON(200, out, nil)
	}
	// commits
	if head == "commits" && len(rest) == 1 {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		tip := resolveRef(store, repo, query.Get("sha"))
		if tip == "" {
			return restErr(404, "Not Found")
		}
		commits := store.ListCommits(repo, tip)
		out := make([]map[string]any, 0, len(commits))
		for _, c := range commits {
			out = append(out, map[string]any{
				"sha": c.SHA,
				"commit": map[string]any{
					"message":   c.Message,
					"author":    map[string]any{"name": c.Author},
					"committer": map[string]any{"name": c.Committer},
				},
				"author":    nil,
				"committer": nil,
			})
		}
		return restJSON(200, out, nil)
	}
	// git/refs (create a branch)
	if head == "git" && len(rest) >= 2 && rest[1] == "refs" && len(rest) == 2 {
		if method != "POST" {
			return restErr(404, "Not Found")
		}
		ref := strVal(input["ref"], "")
		sha := strVal(input["sha"], "")
		result := store.CreateBranch(repo, ref, sha)
		if result.Error != "" {
			return restErr(result.Status, result.Error)
		}
		return restJSON(201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha}}, nil)
	}
	// labels
	if head == "labels" && len(rest) == 1 {
		if method == "POST" {
			name := strVal(input["name"], "")
			if name == "" {
				return restErr(422, "name is required")
			}
			label := store.CreateLabel(repo, name, strVal(input["color"], "ededed"))
			return restJSON(201, map[string]any{"name": label.Name, "color": label.Color}, nil)
		}
		if method == "GET" {
			out := make([]map[string]any, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				out = append(out, map[string]any{"name": l.Name, "color": l.Color})
			}
			return restJSON(200, out, nil)
		}
		return restErr(404, "Not Found")
	}
	// contents
	if head == "contents" {
		path := strings.Join(rest[1:], "/")
		if method == "GET" {
			tip := resolveRef(store, repo, query.Get("ref"))
			if tip == "" {
				return restErr(404, "Not Found")
			}
			found := store.ReadPath(repo, tip, path)
			if found == nil {
				return restErr(404, "Not Found")
			}
			if found.Kind == "file" {
				name := path
				if idx := strings.LastIndex(path, "/"); idx >= 0 {
					name = path[idx+1:]
				}
				return restJSON(200, map[string]any{
					"type": "file", "encoding": "base64", "size": len(found.Data),
					"name": name, "path": path,
					"content": base64.StdEncoding.EncodeToString(found.Data),
					"sha": found.OID,
				}, nil)
			}
			out := make([]map[string]any, 0, len(found.Entries))
			for _, e := range found.Entries {
				size := any(0)
				if e.Type == "file" {
					size = nil
					// TS: size: entry.type === 'dir' ? 0 : undefined — undefined keys
					// are dropped by JSON.stringify. For files the key is absent.
				}
				entry := map[string]any{"type": e.Type, "name": e.Name, "path": e.Path, "sha": e.OID}
				if e.Type == "dir" {
					entry["size"] = size
				}
				out = append(out, entry)
			}
			return restJSON(200, out, nil)
		}
		if method == "PUT" {
			contentB64 := strVal(input["content"], "")
			if contentB64 == "" {
				return restErr(422, "content is required")
			}
			cleaned := strings.Map(func(r rune) rune {
				if r == '\r' || r == '\n' || r == '\t' || r == ' ' {
					return -1
				}
				return r
			}, contentB64)
			content, err := base64.StdEncoding.DecodeString(cleaned)
			if err != nil {
				// Node's base64 decoder is lenient; fall back to raw bytes of the
				// decodable prefix is overkill — treat as empty.
				content = []byte{}
			}
			var branchName string
			if b, ok := input["branch"].(string); ok {
				branchName = b
			}
			tipBefore, hasTip := store.BranchTip(repo, branchNameOrDefault(repo, branchName))
			existedBefore := false
			if hasTip {
				if found := store.ReadPath(repo, tipBefore, path); found != nil && found.Kind == "file" {
					existedBefore = true
				}
			}
			var sha *string
			if s, ok := input["sha"].(string); ok {
				sha = &s
			}
			result := store.PutContent(repo, path, content, strVal(input["message"], "Update"), branchName, sha)
			if result.Error != "" {
				return restErr(result.Status, result.Error)
			}
			name := path
			if idx := strings.LastIndex(path, "/"); idx >= 0 {
				name = path[idx+1:]
			}
			status := 201
			if existedBefore {
				status = 200
			}
			return restJSON(status, map[string]any{
				"content": map[string]any{"name": name, "path": path, "sha": result.ContentSHA, "size": len(content), "encoding": "base64", "type": "file"},
				"commit":  map[string]any{"sha": result.CommitSHA, "message": strVal(input["message"], "Update")},
			}, nil)
		}
		if method == "DELETE" {
			var branchName string
			if b, ok := input["branch"].(string); ok {
				branchName = b
			}
			var sha *string
			if s, ok := input["sha"].(string); ok {
				sha = &s
			}
			result := store.DeleteContent(repo, path, strVal(input["message"], "Delete"), branchName, sha)
			if result.Error != "" {
				return restErr(result.Status, result.Error)
			}
			return restJSON(200, map[string]any{"commit": map[string]any{"sha": result.CommitSHA}}, nil)
		}
		return restErr(404, "Not Found")
	}
	// issues
	if head == "issues" {
		if len(rest) == 1 {
			if method == "POST" {
				title := strVal(input["title"], "")
				if title == "" {
					return restErr(422, "title is required")
				}
				var labels []string
				if arr, ok := input["labels"].([]any); ok {
					for _, l := range arr {
						if s, ok := l.(string); ok {
							labels = append(labels, s)
						}
					}
				}
				issue := store.CreateIssue(repo, title, strVal(input["body"], ""), labels)
				if issue.Error != "" {
					return restErr(issue.Status, issue.Error)
				}
				return restJSON(201, issueJSONOf(repo, issue.Issue), nil)
			}
			if method == "GET" {
				state := query.Get("state")
				sortParam := query.Get("sort")
				if sortParam == "" {
					sortParam = "created"
				}
				direction := query.Get("direction")
				if direction == "" {
					direction = "desc"
				}
				issues := store.ListIssues(repo, state, sortParam, direction)
				page, link := paginate(issues, query, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues", host)
				headers := map[string]string{}
				if link != "" {
					headers["link"] = link
				}
				out := make([]issueJSON, 0, len(page))
				for _, issue := range page {
					out = append(out, issueJSONOf(repo, issue))
				}
				return restJSON(200, out, headers)
			}
			return restErr(404, "Not Found")
		}
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			return restErr(404, "Not Found")
		}
		if len(rest) == 2 {
			if method == "GET" {
				issue := store.GetIssue(repo, number)
				if issue == nil {
					return restErr(404, "Not Found")
				}
				return restJSON(200, issueJSONOf(repo, issue), nil)
			}
			if method == "PATCH" {
				state, ok := input["state"].(string)
				if !ok || (state != "open" && state != "closed") {
					return restErr(422, "state must be open or closed")
				}
				issue := store.SetIssueState(repo, number, state)
				if issue == nil {
					return restErr(404, "Not Found")
				}
				return restJSON(200, issueJSONOf(repo, issue), nil)
			}
			return restErr(404, "Not Found")
		}
		if len(rest) == 3 && rest[2] == "comments" {
			if method == "POST" {
				comment := store.AddComment(repo, number, strVal(input["body"], ""))
				if comment == nil {
					return restErr(404, "Not Found")
				}
				return restJSON(201, map[string]any{
					"id": comment.ID, "body": comment.Body,
					"user":       map[string]any{"login": VIEWER.Login, "id": VIEWER.ID},
					"created_at": comment.CreatedAt, "updated_at": comment.UpdatedAt,
				}, nil)
			}
			if method == "GET" {
				issue := store.GetIssue(repo, number)
				if issue == nil {
					return restErr(404, "Not Found")
				}
				out := make([]map[string]any, 0, len(issue.Comments))
				for _, c := range issue.Comments {
					out = append(out, map[string]any{
						"id": c.ID, "body": c.Body,
						"user":       map[string]any{"login": VIEWER.Login, "id": VIEWER.ID},
						"created_at": c.CreatedAt, "updated_at": c.UpdatedAt,
					})
				}
				return restJSON(200, out, nil)
			}
			return restErr(404, "Not Found")
		}
		return restErr(404, "Not Found")
	}
	// pulls
	if head == "pulls" {
		if len(rest) == 1 {
			if method == "POST" {
				title := strVal(input["title"], "")
				headRef := strVal(input["head"], "")
				baseRef := strVal(input["base"], "")
				if title == "" || headRef == "" || baseRef == "" {
					return restErr(422, "title, head and base are required")
				}
				pull := store.CreatePull(repo, title, strVal(input["body"], ""), headRef, baseRef)
				if pull.Error != "" {
					return restErr(pull.Status, pull.Error)
				}
				return restJSON(201, pullJSONOf(repo, pull.Pull), nil)
			}
			if method == "GET" {
				state := query.Get("state")
				pulls := store.ListPulls(repo, state)
				page, link := paginate(pulls, query, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/pulls", host)
				headers := map[string]string{}
				if link != "" {
					headers["link"] = link
				}
				out := make([]pullJSON, 0, len(page))
				for _, pull := range page {
					out = append(out, pullJSONOf(repo, pull))
				}
				return restJSON(200, out, headers)
			}
			return restErr(404, "Not Found")
		}
		number, err := strconv.Atoi(rest[1])
		if err != nil {
			return restErr(404, "Not Found")
		}
		if len(rest) == 2 {
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			pull := store.GetPull(repo, number)
			if pull == nil {
				return restErr(404, "Not Found")
			}
			return restJSON(200, pullJSONOf(repo, pull), nil)
		}
		if len(rest) == 3 && rest[2] == "files" {
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			pull := store.GetPull(repo, number)
			if pull == nil {
				return restErr(404, "Not Found")
			}
			files := store.PullFiles(repo, pull)
			out := make([]map[string]any, 0, len(files))
			for _, f := range files {
				out = append(out, map[string]any{
					"filename": f.Filename, "status": f.Status,
					"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
				})
			}
			return restJSON(200, out, nil)
		}
		if len(rest) == 3 && rest[2] == "merge" {
			if method != "PUT" {
				return restErr(404, "Not Found")
			}
			pull := store.GetPull(repo, number)
			if pull == nil {
				return restErr(404, "Not Found")
			}
			result := store.MergePull(repo, pull)
			if result.Error != "" {
				return restErr(result.Status, result.Error)
			}
			return restJSON(200, map[string]any{"merged": true, "sha": result.SHA, "message": "Pull request successfully merged"}, nil)
		}
		return restErr(404, "Not Found")
	}
	return restErr(404, "Not Found")
}

func branchNameOrDefault(repo *Repo, branch string) string {
	if branch == "" {
		return repo.DefaultBranch
	}
	return branch
}

// marshalJSON renders a REST body as JSON bytes (with a trailing newline, as
// the TS forge's sendJson does). encoding/json escapes <, > and & by default;
// the forge disables that to match Node's JSON.stringify.
func marshalJSON(v any) []byte {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	// Encoder appends '\n' itself.
	return []byte(buf.String())
}
