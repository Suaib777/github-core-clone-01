// store.go — the forge's in-memory store: users, repositories, git objects,
// issues, labels, pull requests and comments. One store per process; everything
// the surfaces read and write lives here, so REST, GraphQL, MCP and git all see
// the same state. Ported from the TypeScript forge (store.ts).
//
// Determinism: no wall clock, no randomness. Every timestamp comes from a single
// monotonic tick seeded by MAKEFUN_TRIAL_NOW (the instance's opening time,
// identical in live and replay), so two runs of the same request sequence produce
// byte-identical git objects.
//
// Concurrency: the HTTP server runs handlers in parallel (the clone workload is
// 50 concurrent clones). The store is guarded by one RWMutex: read-only handlers
// take the read lock, mutating handlers take the write lock. The deterministic
// tick is advanced only under the write lock, so its order follows request order.

package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type User struct {
	Login string
	ID    int
	Name  string
	Email string
}

// VIEWER is the trial's viewer, from the scenario's fixed seed.
var VIEWER = User{Login: "adaiversen", ID: 1000, Name: "ada iversen", Email: "adaiversen@example.invalid"}

const DefaultBranch = "main"

var ForgeIdentity = fmt.Sprintf("%s <%s>", VIEWER.Name, VIEWER.Email)

var baseTimeSec int64 = 1700000000

func init() {
	if v := os.Getenv("MAKEFUN_TRIAL_NOW"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			baseTimeSec = ms / 1000
		}
	}
}

// tickMu guards tick; tick advances only while the store write lock is held.
var tickMu sync.Mutex
var tick int64

func nowSec() int64 {
	tickMu.Lock()
	defer tickMu.Unlock()
	s := baseTimeSec + tick
	tick++
	return s
}

func nowIso() string {
	return time.Unix(nowSec(), 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

type Label struct {
	Name  string
	Color string
}

type Comment struct {
	ID        int
	Body      string
	User      User
	CreatedAt string
	UpdatedAt string
}

type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	Comments  []*Comment
	User      User
	CreatedAt string
	UpdatedAt string
	ClosedAt  *string
}

type PullFile struct {
	Filename  string
	Status    string // "added" | "removed" | "modified"
	Additions int
	Deletions int
	Changes   int
}

type Pull struct {
	Number        int
	Title         string
	Body          string
	State         string // "open" | "closed"
	HeadRef       string
	HeadSHA       string
	BaseRef       string
	BaseSHA       string
	Merged        bool
	MergedAt      *string
	MergeCommitSHA *string
	User          User
	CreatedAt     string
	UpdatedAt     string
}

type Repo struct {
	ID           int
	DatabaseID   int
	Name         string
	Owner        string
	Description  string
	Private      bool
	DefaultBranch string
	CreatedAt    string
	UpdatedAt    string
	// refs/heads/<name> and refs/tags/<name> -> commit/tag oid.
	Refs map[string]string
	// oid -> object.
	Objects map[string]*GitObject
	Labels  []*Label
	Issues  []*Issue
	Pulls   []*Pull
	// Shared issue/PR number sequence (GitHub numbers issues and PRs together).
	Seq        int
	CommentSeq int
}

type Store struct {
	mu      sync.RWMutex
	repos   map[string]*Repo
	repoSeq int
}

func NewStore() *Store {
	return &Store{repos: make(map[string]*Repo)}
}

func (s *Store) RLock()   { s.mu.RLock() }
func (s *Store) RUnlock() { s.mu.RUnlock() }
func (s *Store) Lock()    { s.mu.Lock() }
func (s *Store) Unlock()  { s.mu.Unlock() }

func (s *Store) key(owner, name string) string { return owner + "/" + name }

// GetRepo returns the repo, or nil. Callers hold the lock.
func (s *Store) GetRepo(owner, name string) *Repo {
	return s.repos[s.key(owner, name)]
}

func (s *Store) CreateRepo(name string, private bool, description string) *Repo {
	s.repoSeq++
	id := s.repoSeq
	now := nowIso()
	repo := &Repo{
		ID:            id,
		DatabaseID:    id,
		Name:          name,
		Owner:         VIEWER.Login,
		Description:   description,
		Private:       private,
		DefaultBranch: DefaultBranch,
		CreatedAt:     now,
		UpdatedAt:     now,
		Refs:          make(map[string]string),
		Objects:       make(map[string]*GitObject),
	}
	s.repos[s.key(VIEWER.Login, name)] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	_, ok := s.repos[s.key(owner, name)]
	if ok {
		delete(s.repos, s.key(owner, name))
	}
	return ok
}

// ViewerRepos returns all repositories owned by the viewer, in creation order.
func (s *Store) ViewerRepos() []*Repo {
	var out []*Repo
	for _, repo := range s.repos {
		if repo.Owner == VIEWER.Login {
			out = append(out, repo)
		}
	}
	// Creation order = id order (map order is random in Go).
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Repos returns all repos (for GraphQL node lookups).
func (s *Store) Repos() []*Repo {
	out := make([]*Repo, 0, len(s.repos))
	for _, repo := range s.repos {
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------------------------------------------------------------------------
// git objects
// ---------------------------------------------------------------------------

// PutObject registers an object in a repo (idempotent) and returns its oid.
func (s *Store) PutObject(repo *Repo, typ GitObjectType, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		repo.Objects[oid] = &GitObject{Type: typ, Data: cp}
	}
	return oid
}

func (s *Store) Lookup(repo *Repo) ObjectLookup {
	return func(oid string) *GitObject { return repo.Objects[oid] }
}

func (s *Store) RefTip(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	return oid, ok
}

func (s *Store) BranchTip(repo *Repo, branch string) (string, bool) {
	oid, ok := repo.Refs["refs/heads/"+branch]
	return oid, ok
}

// DefaultTip is the tip of the repo's default branch, or "" when unborn.
func (s *Store) DefaultTip(repo *Repo) string {
	return repo.Refs["refs/heads/"+repo.DefaultBranch]
}

// MakeCommit creates a commit object for a tree with the given parents and message.
func (s *Store) MakeCommit(repo *Repo, tree string, parents []string, message string) string {
	stamp := fmt.Sprintf("%d +0000", nowSec())
	text := message
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	data := BuildCommit(CommitInfo{
		Tree:      tree,
		Parents:   append([]string(nil), parents...),
		Author:    ForgeIdentity + " " + stamp,
		Committer: ForgeIdentity + " " + stamp,
		Message:   text,
	})
	return s.PutObject(repo, ObjCommit, data)
}

// FlatTree returns the flat file map of a commit's tree.
func (s *Store) FlatTree(repo *Repo, commitOID string) map[string]FlatEntry {
	commit := repo.Objects[commitOID]
	if commit == nil || commit.Type != ObjCommit {
		return make(map[string]FlatEntry)
	}
	return FlattenTree(ParseCommit(commit.Data).Tree, s.Lookup(repo))
}

// FlatTreeOID returns the flat file map of a tree oid.
func (s *Store) FlatTreeOID(repo *Repo, treeOID string) map[string]FlatEntry {
	return FlattenTree(treeOID, s.Lookup(repo))
}

// BuildTree builds a new tree from a flat file map, registering every tree object.
func (s *Store) BuildTree(repo *Repo, files map[string]FlatEntry) string {
	return BuildTreeFromMap(files, func(typ GitObjectType, data []byte) string {
		return s.PutObject(repo, typ, data)
	})
}

// ---------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------

type ReadPathResult struct {
	Kind    string // "file" | "dir"
	Mode    string
	OID     string
	Data    []byte
	Entries []DirEntry
}

type DirEntry struct {
	Name string
	Path string
	Type string // "file" | "dir"
	OID  string
}

// ReadPath reads a file or directory from a commit. Returns nil when absent.
func (s *Store) ReadPath(repo *Repo, commitOID, path string) *ReadPathResult {
	commit := repo.Objects[commitOID]
	if commit == nil || commit.Type != ObjCommit {
		return nil
	}
	rootTree := ParseCommit(commit.Data).Tree
	if path == "" {
		entries := s.DirEntries(repo, rootTree, "")
		if entries == nil {
			return nil
		}
		return &ReadPathResult{Kind: "dir", Entries: entries}
	}
	files := s.FlatTree(repo, commitOID)
	if file, ok := files[path]; ok {
		obj := repo.Objects[file.OID]
		if obj == nil {
			return nil
		}
		return &ReadPathResult{Kind: "file", Mode: file.Mode, OID: file.OID, Data: obj.Data}
	}
	dirTree := s.DirOID(repo, rootTree, path)
	if dirTree == "" {
		return nil
	}
	entries := s.DirEntries(repo, dirTree, path)
	if entries == nil {
		return nil
	}
	return &ReadPathResult{Kind: "dir", Entries: entries}
}

// DirEntries returns the entries of one tree object, with full paths.
func (s *Store) DirEntries(repo *Repo, treeOID, dirPath string) []DirEntry {
	obj := repo.Objects[treeOID]
	if obj == nil || obj.Type != ObjTree {
		return nil
	}
	entries, err := ParseTree(obj.Data)
	if err != nil {
		return nil
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		path := e.Name
		if dirPath != "" {
			path = dirPath + "/" + e.Name
		}
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		out = append(out, DirEntry{Name: e.Name, Path: path, Type: typ, OID: e.OID})
	}
	return out
}

// DirOID returns the tree oid of a directory path within a tree ("" = root), or "" when absent.
func (s *Store) DirOID(repo *Repo, treeOID, dirPath string) string {
	if dirPath == "" {
		return treeOID
	}
	current := treeOID
	for _, segment := range strings.Split(dirPath, "/") {
		obj := repo.Objects[current]
		if obj == nil || obj.Type != ObjTree {
			return ""
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return ""
		}
		found := ""
		for _, e := range entries {
			if e.Name == segment {
				if e.Mode != "40000" {
					return ""
				}
				found = e.OID
				break
			}
		}
		if found == "" {
			return ""
		}
		current = found
	}
	return current
}

// ---------------------------------------------------------------------------
// contents: create / update / delete a file, each as one commit
// ---------------------------------------------------------------------------

// ContentsRef returns the ref a contents call targets.
func (s *Store) ContentsRef(repo *Repo, branch string) string {
	if branch == "" {
		branch = repo.DefaultBranch
	}
	return "refs/heads/" + branch
}

type PutContentResult struct {
	CommitSHA  string
	ContentSHA string
	Error      string
	Status     int
}

// PutContent creates or updates a file. sha is the blob the caller read.
func (s *Store) PutContent(repo *Repo, path string, content []byte, message string, branch string, sha *string) PutContentResult {
	ref := s.ContentsRef(repo, branch)
	tip, hasTip := repo.Refs[ref]
	var files map[string]FlatEntry
	if hasTip {
		files = copyFlat(s.FlatTree(repo, tip))
	} else {
		files = make(map[string]FlatEntry)
	}
	if existing, ok := files[path]; ok && hasTip {
		if sha == nil {
			return PutContentResult{Error: "sha is required to update a file", Status: 409}
		}
		if !strings.EqualFold(*sha, existing.OID) {
			return PutContentResult{Error: "sha does not match the current blob", Status: 409}
		}
	}
	blobOID := s.PutObject(repo, ObjBlob, content)
	files[path] = FlatEntry{Mode: "100644", OID: blobOID}
	treeOID := s.BuildTree(repo, files)
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitSHA := s.MakeCommit(repo, treeOID, parents, message)
	repo.Refs[ref] = commitSHA
	repo.UpdatedAt = nowIso()
	return PutContentResult{CommitSHA: commitSHA, ContentSHA: blobOID}
}

type DeleteContentResult struct {
	CommitSHA string
	Error     string
	Status    int
}

// DeleteContent deletes a file.
func (s *Store) DeleteContent(repo *Repo, path string, message string, branch string, sha *string) DeleteContentResult {
	ref := s.ContentsRef(repo, branch)
	tip, hasTip := repo.Refs[ref]
	if !hasTip {
		return DeleteContentResult{Error: "Not Found", Status: 404}
	}
	files := copyFlat(s.FlatTree(repo, tip))
	existing, ok := files[path]
	if !ok {
		return DeleteContentResult{Error: "Not Found", Status: 404}
	}
	if sha != nil && !strings.EqualFold(*sha, existing.OID) {
		return DeleteContentResult{Error: "sha does not match the current blob", Status: 409}
	}
	delete(files, path)
	treeOID := s.BuildTree(repo, files)
	commitSHA := s.MakeCommit(repo, treeOID, []string{tip}, message)
	repo.Refs[ref] = commitSHA
	repo.UpdatedAt = nowIso()
	return DeleteContentResult{CommitSHA: commitSHA}
}

func copyFlat(m map[string]FlatEntry) map[string]FlatEntry {
	out := make(map[string]FlatEntry, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// commits and branches
// ---------------------------------------------------------------------------

type CommitRow struct {
	SHA       string
	Message   string
	Author    string
	Committer string
}

// ListCommits returns commits reachable from a tip, newest first (parents after children).
func (s *Store) ListCommits(repo *Repo, tip string) []CommitRow {
	if tip == "" {
		return nil
	}
	var out []CommitRow
	seen := make(map[string]bool)
	queue := []string{tip}
	for qi := 0; qi < len(queue); qi++ {
		oid := queue[qi]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		info := ParseCommit(obj.Data)
		message := info.Message
		if strings.HasSuffix(message, "\n") {
			message = message[:len(message)-1]
		}
		out = append(out, CommitRow{SHA: oid, Message: message, Author: info.Author, Committer: info.Committer})
		queue = append(queue, info.Parents...)
	}
	return out
}

type BranchRow struct {
	Name string
	SHA  string
}

func (s *Store) ListBranches(repo *Repo) []BranchRow {
	var out []BranchRow
	for ref, sha := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			out = append(out, BranchRow{Name: ref[len("refs/heads/"):], SHA: sha})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type BranchResult struct {
	Error  string
	Status int
}

// CreateBranch creates a branch or tag ref.
func (s *Store) CreateBranch(repo *Repo, ref, sha string) BranchResult {
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		return BranchResult{Error: "ref must start with refs/heads/ or refs/tags/", Status: 422}
	}
	if _, ok := repo.Objects[sha]; !ok && sha != ZeroOID {
		return BranchResult{Error: "object not found", Status: 422}
	}
	if _, ok := repo.Refs[ref]; ok {
		return BranchResult{Error: "Reference already exists", Status: 422}
	}
	repo.Refs[ref] = sha
	return BranchResult{}
}

// ---------------------------------------------------------------------------
// labels, issues, comments
// ---------------------------------------------------------------------------

func (s *Store) CreateLabel(repo *Repo, name, color string) *Label {
	for _, l := range repo.Labels {
		if l.Name == name {
			return l
		}
	}
	label := &Label{Name: name, Color: color}
	repo.Labels = append(repo.Labels, label)
	return label
}

type IssueResult struct {
	Issue  *Issue
	Error  string
	Status int
}

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) IssueResult {
	for _, name := range labels {
		found := false
		for _, l := range repo.Labels {
			if l.Name == name {
				found = true
				break
			}
		}
		if !found {
			return IssueResult{Error: "Label does not exist: " + name, Status: 422}
		}
	}
	repo.Seq++
	now := nowIso()
	issue := &Issue{
		Number:    repo.Seq,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string(nil), labels...),
		User:      VIEWER,
		CreatedAt: now,
		UpdatedAt: now,
	}
	repo.Issues = append(repo.Issues, issue)
	return IssueResult{Issue: issue}
}

func (s *Store) GetIssue(repo *Repo, number int) *Issue {
	for _, issue := range repo.Issues {
		if issue.Number == number {
			return issue
		}
	}
	return nil
}

func (s *Store) ListIssues(repo *Repo, state, sortParam, direction string) []*Issue {
	list := append([]*Issue(nil), repo.Issues...)
	if state != "" && state != "all" {
		filtered := list[:0]
		for _, issue := range list {
			if issue.State == state {
				filtered = append(filtered, issue)
			}
		}
		list = filtered
	}
	dir := -1
	if direction == "asc" {
		dir = 1
	}
	// Both sort modes sort by number (as in the TS forge).
	sort.SliceStable(list, func(i, j int) bool {
		return (list[i].Number-list[j].Number)*dir < 0
	})
	return list
}

func (s *Store) SetIssueState(repo *Repo, number int, state string) *Issue {
	issue := s.GetIssue(repo, number)
	if issue == nil {
		return nil
	}
	issue.State = state
	issue.UpdatedAt = nowIso()
	if state == "closed" {
		issue.ClosedAt = &issue.UpdatedAt
	} else {
		issue.ClosedAt = nil
	}
	return issue
}

func (s *Store) AddComment(repo *Repo, issueNumber int, body string) *Comment {
	issue := s.GetIssue(repo, issueNumber)
	if issue == nil {
		return nil
	}
	repo.CommentSeq++
	now := nowIso()
	comment := &Comment{ID: repo.CommentSeq, Body: body, User: VIEWER, CreatedAt: now, UpdatedAt: now}
	issue.Comments = append(issue.Comments, comment)
	return comment
}

// ---------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------

type PullResult struct {
	Pull   *Pull
	Error  string
	Status int
}

func (s *Store) CreatePull(repo *Repo, title, body, head, base string) PullResult {
	headTip, ok1 := repo.Refs["refs/heads/"+head]
	baseTip, ok2 := repo.Refs["refs/heads/"+base]
	if !ok1 {
		return PullResult{Error: "head branch not found: " + head, Status: 422}
	}
	if !ok2 {
		return PullResult{Error: "base branch not found: " + base, Status: 422}
	}
	repo.Seq++
	now := nowIso()
	pull := &Pull{
		Number:    repo.Seq,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   head,
		HeadSHA:   headTip,
		BaseRef:   base,
		BaseSHA:   baseTip,
		User:      VIEWER,
		CreatedAt: now,
		UpdatedAt: now,
	}
	repo.Pulls = append(repo.Pulls, pull)
	return PullResult{Pull: pull}
}

func (s *Store) GetPull(repo *Repo, number int) *Pull {
	for _, pull := range repo.Pulls {
		if pull.Number == number {
			return pull
		}
	}
	return nil
}

func (s *Store) ListPulls(repo *Repo, state string) []*Pull {
	list := append([]*Pull(nil), repo.Pulls...)
	if state == "" || state == "open" {
		filtered := list[:0]
		for _, pull := range list {
			if pull.State == "open" {
				filtered = append(filtered, pull)
			}
		}
		list = filtered
	} else if state == "closed" {
		filtered := list[:0]
		for _, pull := range list {
			if pull.State == "closed" {
				filtered = append(filtered, pull)
			}
		}
		list = filtered
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	return list
}

// PullFiles returns the files a pull request changes (merge-base vs head).
func (s *Store) PullFiles(repo *Repo, pull *Pull) []PullFile {
	headTip := pull.HeadSHA
	if v, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		headTip = v
	}
	baseTip := pull.BaseSHA
	if v, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseTip = v
	}
	base := MergeBase(baseTip, headTip, s.Lookup(repo))
	if base == "" {
		base = baseTip
	}
	baseCommit := repo.Objects[base]
	headCommit := repo.Objects[headTip]
	if baseCommit == nil || headCommit == nil {
		return nil
	}
	baseTree := ParseCommit(baseCommit.Data).Tree
	headTree := ParseCommit(headCommit.Data).Tree
	changes := DiffTrees(baseTree, headTree, s.Lookup(repo))
	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var out []PullFile
	for _, path := range paths {
		change := changes[path]
		status := "modified"
		if !change.HasNew {
			status = "removed"
		} else if !change.HasOld {
			status = "added"
		}
		additions, deletions := 0, 0
		oldText := ""
		if change.HasOld {
			if obj := repo.Objects[change.OldOID]; obj != nil {
				oldText = string(obj.Data)
			}
		}
		newText := ""
		if change.HasNew {
			if obj := repo.Objects[change.NewOID]; obj != nil {
				newText = string(obj.Data)
			}
		}
		var oldLines, newLines []string
		if oldText != "" {
			oldLines = strings.Split(oldText, "\n")
		}
		if newText != "" {
			newLines = strings.Split(newText, "\n")
		}
		oldSet := make(map[string]bool, len(oldLines))
		for _, l := range oldLines {
			oldSet[l] = true
		}
		newSet := make(map[string]bool, len(newLines))
		for _, l := range newLines {
			newSet[l] = true
		}
		for _, l := range newLines {
			if !oldSet[l] {
				additions++
			}
		}
		for _, l := range oldLines {
			if !newSet[l] {
				deletions++
			}
		}
		if status == "added" && additions < len(newLines) {
			additions = len(newLines)
		}
		if status == "removed" && deletions < len(oldLines) {
			deletions = len(oldLines)
		}
		out = append(out, PullFile{Filename: path, Status: status, Additions: additions, Deletions: deletions, Changes: additions + deletions})
	}
	return out
}

// Mergeable reports whether a pull request can merge cleanly.
func (s *Store) Mergeable(repo *Repo, pull *Pull) bool {
	if pull.State != "open" {
		return false
	}
	preview := s.MergePreview(repo, pull)
	return preview.Conflict == ""
}

type MergePreview struct {
	Conflict string // "" when clean
	Files    map[string]FlatEntry // nil when conflict
	HasFiles bool
}

// MergePreview computes the merge preview: the merged file map, or the conflict.
func (s *Store) MergePreview(repo *Repo, pull *Pull) MergePreview {
	headTip := pull.HeadSHA
	if v, ok := repo.Refs["refs/heads/"+pull.HeadRef]; ok {
		headTip = v
	}
	baseTip := pull.BaseSHA
	if v, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseTip = v
	}
	base := MergeBase(baseTip, headTip, s.Lookup(repo))
	if base == "" {
		return MergePreview{Files: s.FlatTree(repo, headTip), HasFiles: true}
	}
	baseCommit := repo.Objects[base]
	baseTipCommit := repo.Objects[baseTip]
	headCommit := repo.Objects[headTip]
	if baseCommit == nil || baseTipCommit == nil || headCommit == nil {
		return MergePreview{}
	}
	baseTree := ParseCommit(baseCommit.Data).Tree
	baseTipTree := ParseCommit(baseTipCommit.Data).Tree
	headTree := ParseCommit(headCommit.Data).Tree
	lookup := s.Lookup(repo)
	baseChanges := DiffTrees(baseTree, baseTipTree, lookup)
	headChanges := DiffTrees(baseTree, headTree, lookup)
	// Deterministic order: sorted paths (the TS forge walks a Map in flatten
	// order; a single conflict is the case the suite exercises).
	headPaths := make([]string, 0, len(headChanges))
	for path := range headChanges {
		headPaths = append(headPaths, path)
	}
	sort.Strings(headPaths)
	for _, path := range headPaths {
		headChange := headChanges[path]
		if baseChange, ok := baseChanges[path]; ok && baseChange.NewOID != headChange.NewOID {
			return MergePreview{Conflict: path}
		}
	}
	files := s.FlatTree(repo, baseTip)
	for _, path := range headPaths {
		change := headChanges[path]
		if !change.HasNew {
			delete(files, path)
		} else {
			files[path] = FlatEntry{Mode: "100644", OID: change.NewOID}
		}
	}
	return MergePreview{Files: files, HasFiles: true}
}

type MergeResult struct {
	SHA    string
	Error  string
	Status int
}

// MergePull squash-merges a pull request onto its base branch.
func (s *Store) MergePull(repo *Repo, pull *Pull) MergeResult {
	if pull.State != "open" {
		return MergeResult{Error: "pull request is not open", Status: 405}
	}
	preview := s.MergePreview(repo, pull)
	if preview.Conflict != "" || !preview.HasFiles {
		conflict := preview.Conflict
		if conflict == "" {
			conflict = "unknown path"
		}
		return MergeResult{Error: "merge conflict in " + conflict, Status: 405}
	}
	baseTip := pull.BaseSHA
	if v, ok := repo.Refs["refs/heads/"+pull.BaseRef]; ok {
		baseTip = v
	}
	treeOID := s.BuildTree(repo, preview.Files)
	message := fmt.Sprintf("Merge pull request #%d from %s/%s", pull.Number, repo.Owner, pull.HeadRef)
	mergeSHA := s.MakeCommit(repo, treeOID, []string{baseTip}, message)
	repo.Refs["refs/heads/"+pull.BaseRef] = mergeSHA
	pull.State = "closed"
	pull.Merged = true
	mergedAt := nowIso()
	pull.MergedAt = &mergedAt
	pull.MergeCommitSHA = &mergeSHA
	pull.UpdatedAt = mergedAt
	repo.UpdatedAt = mergedAt
	return MergeResult{SHA: mergeSHA}
}
