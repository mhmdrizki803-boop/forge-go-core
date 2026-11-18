package main

// Deterministic in-memory persistence for every forge API: ids come from counters,
// every timestamp is EPOCH, and git objects keep their canonical uncompressed bodies.

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// EPOCH is the one timestamp the store writes: a replay has to be byte-identical.
const EPOCH = "2026-01-01T00:00:00Z"

// EpochSeconds is EPOCH as unix seconds; commits carry it in their identity lines.
const EpochSeconds = 1767225600

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

// hashObject is the git object id of "<type> <len>\0<body>".
func hashObject(typ string, body []byte) string {
	hdr := []byte(typ + " " + strconv.Itoa(len(body)) + "\x00")
	buf := make([]byte, 0, len(hdr)+len(body))
	buf = append(buf, hdr...)
	buf = append(buf, body...)
	return sha1Hex(buf)
}

// nodeID is a deterministic stand-in for GitHub's opaque node ids.
func nodeID(kind string, id int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + strconv.Itoa(id)))
}

// GitObject is a stored git object with its type and uncompressed body.
type GitObject struct {
	Type string
	Body []byte
}

// UserRecord is a forge user and its stable API metadata.
type UserRecord struct {
	Login     string
	ID        int
	NodeID    string
	Name      string
	Email     string
	Type      string
	SiteAdmin bool
	AvatarURL string
	HTMLURL   string
	URL       string
}

// LabelRecord is a repository issue label.
type LabelRecord struct {
	ID          int
	NodeID      string
	Name        string
	Color       string
	Description *string
	Default     bool
	URL         string
}

// IssueRecord is a repository issue.
type IssueRecord struct {
	ID                int
	NodeID            string
	Number            int
	Title             string
	Body              string
	State             string
	StateReason       *string
	Locked            bool
	Labels            []*LabelRecord
	User              *UserRecord
	Comments          int
	CreatedAt         string
	UpdatedAt         string
	ClosedAt          *string
	AuthorAssociation string
	HTMLURL           string
	URL               string
	RepositoryURL     string
	CommentsURL       string
	EventsURL         string
	LabelsURL         string
	TimelineURL       string
}

// PullRef is one side of a pull request: its ref, sha, label and user.
type PullRef struct {
	Ref   string
	Sha   string
	Label string
	User  *UserRecord
}

// PullRecord is a repository pull request.
type PullRecord struct {
	ID                  int
	NodeID              string
	Number              int
	Title               string
	Body                string
	State               string
	Locked              bool
	Draft               bool
	Merged              bool
	MergedAt            *string
	MergeCommitSha      *string
	Mergeable           *bool
	MergeableState      string
	Rebaseable          bool
	User                *UserRecord
	Head                PullRef
	Base                PullRef
	CreatedAt           string
	UpdatedAt           string
	ClosedAt            *string
	Commits             int
	Additions           int
	Deletions           int
	ChangedFiles        int
	Comments            int
	ReviewComments      int
	MaintainerCanModify bool
	AuthorAssociation   string
	URL                 string
	HTMLURL             string
	DiffURL             string
	PatchURL            string
	IssueURL            string
	CommentsURL         string
	StatusesURL         string
	CommitsURL          string
	ReviewCommentsURL   string
	RepositoryURL       string
}

// CommentRecord is an issue comment.
type CommentRecord struct {
	ID                int
	NodeID            string
	Body              string
	User              *UserRecord
	CreatedAt         string
	UpdatedAt         string
	HTMLURL           string
	URL               string
	IssueURL          string
	AuthorAssociation string
}

// RepoData is the per-repo buckets, kept beside the repo record.
type RepoData struct {
	Objects     map[string]*GitObject
	Refs        map[string]string
	Issues      map[int]*IssueRecord
	Pulls       map[int]*PullRecord
	Labels      map[string]*LabelRecord
	Comments    map[int]*CommentRecord
	NextIssue   int
	NextPull    int
	NextLabel   int
	NextComment int
	Bytes       int
}

// RepoRecord is a repository's public metadata paired with its data buckets.
type RepoRecord struct {
	ID              int
	NodeID          string
	Name            string
	FullName        string
	Owner           *UserRecord
	OwnerLogin      string
	Private         bool
	Description     *string
	DefaultBranch   string
	CreatedAt       string
	UpdatedAt       string
	PushedAt        string
	Size            int
	HTMLURL         string
	URL             string
	CloneURL        string
	SSHURL          string
	GitURL          string
	OpenIssuesCount int
	OpenPullsCount  int
	Fork            bool
	Archived        bool
	Disabled        bool
	Visibility      string
	HasIssues       bool
	HasWiki         bool
	HasProjects     bool
	HasDownloads    bool
	HasPages        bool
	HasDiscussions  bool
	Topics          []string
	Data            *RepoData
}

// Store is everything the forge knows, keyed the way the API addresses it.
type Store struct {
	Base       string
	Users      map[string]*UserRecord
	Repos      map[string]*RepoRecord
	NextUserID int
	NextRepoID int
	// Writes counts every mutation: a cached reply keyed on it is never served after a change.
	Writes int
}

func newStore() *Store {
	return &Store{
		Base:       "http://localhost:8080",
		Users:      map[string]*UserRecord{},
		Repos:      map[string]*RepoRecord{},
		NextUserID: 1,
		NextRepoID: 1,
	}
}

func hostOf(base string) string {
	return strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
}

// CreateUser returns the user for a login, creating it on first sight.
func (s *Store) CreateUser(login string) *UserRecord {
	if u, ok := s.Users[login]; ok {
		return u
	}
	id := s.NextUserID
	s.NextUserID++
	u := &UserRecord{
		Login:     login,
		ID:        id,
		NodeID:    nodeID("User", id),
		Name:      login,
		Email:     login + "@users.noreply.github.com",
		Type:      "User",
		SiteAdmin: false,
		AvatarURL: s.Base + "/avatars/u/" + strconv.Itoa(id),
		HTMLURL:   s.Base + "/" + login,
		URL:       s.Base + "/api/v3/users/" + login,
	}
	s.Users[login] = u
	return u
}

func (s *Store) GetUser(login string) *UserRecord {
	return s.Users[login]
}

// CreateRepo makes a repo record, or returns nil when "owner/name" is taken.
func (s *Store) CreateRepo(owner, name string, description *string, private bool) *RepoRecord {
	ownerUser := s.CreateUser(owner)
	full := owner + "/" + name
	if _, ok := s.Repos[full]; ok {
		return nil
	}
	id := s.NextRepoID
	s.NextRepoID++
	base := s.Base
	visibility := "public"
	if private {
		visibility = "private"
	}
	repo := &RepoRecord{
		ID:            id,
		NodeID:        nodeID("Repository", id),
		Name:          name,
		FullName:      full,
		Owner:         ownerUser,
		OwnerLogin:    owner,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		CreatedAt:     EPOCH,
		UpdatedAt:     EPOCH,
		PushedAt:      EPOCH,
		Size:          0,
		HTMLURL:       base + "/" + full,
		URL:           base + "/api/v3/repos/" + full,
		CloneURL:      base + "/" + full + ".git",
		SSHURL:        "git@" + hostOf(base) + ":" + full + ".git",
		GitURL:        "git://" + hostOf(base) + "/" + full + ".git",
		Visibility:    visibility,
		HasIssues:     true,
		Topics:        []string{},
		Data: &RepoData{
			Objects:     map[string]*GitObject{},
			Refs:        map[string]string{},
			Issues:      map[int]*IssueRecord{},
			Pulls:       map[int]*PullRecord{},
			Labels:      map[string]*LabelRecord{},
			Comments:    map[int]*CommentRecord{},
			NextIssue:   1,
			NextPull:    1,
			NextLabel:   1,
			NextComment: 1,
		},
	}
	s.Repos[full] = repo
	return repo
}

// GetRepo looks up "owner/name".
func (s *Store) GetRepo(owner, name string) *RepoRecord {
	return s.Repos[owner+"/"+name]
}

// GetRepoFull looks up a full "owner/name" key.
func (s *Store) GetRepoFull(full string) *RepoRecord {
	return s.Repos[full]
}

func (s *Store) DeleteRepo(full string) bool {
	s.Writes++
	if _, ok := s.Repos[full]; !ok {
		return false
	}
	delete(s.Repos, full)
	return true
}

// ListRepos returns every repo owned by owner, sorted by name.
func (s *Store) ListRepos(owner string) []*RepoRecord {
	out := make([]*RepoRecord, 0, len(s.Repos))
	for _, repo := range s.Repos {
		if owner == "" || repo.OwnerLogin == owner {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out
}

// PutObject stores an object and returns its oid. Idempotent.
func (s *Store) PutObject(repo *RepoRecord, typ string, body []byte) string {
	oid := hashObject(typ, body)
	if _, ok := repo.Data.Objects[oid]; !ok {
		repo.Data.Objects[oid] = &GitObject{Type: typ, Body: body}
		repo.Data.Bytes += len(body)
		repo.Size = (repo.Data.Bytes + 1023) / 1024
	}
	return oid
}

func (s *Store) GetObject(repo *RepoRecord, oid string) *GitObject {
	return repo.Data.Objects[oid]
}

func (s *Store) HasObject(repo *RepoRecord, oid string) bool {
	_, ok := repo.Data.Objects[oid]
	return ok
}

// SetRef points ref at oid and counts the write.
func (s *Store) SetRef(repo *RepoRecord, ref, oid string) {
	s.Writes++
	repo.Data.Refs[ref] = oid
	repo.UpdatedAt = EPOCH
}

func (s *Store) DeleteRef(repo *RepoRecord, ref string) bool {
	s.Writes++
	if _, ok := repo.Data.Refs[ref]; !ok {
		return false
	}
	delete(repo.Data.Refs, ref)
	return true
}

func (s *Store) GetRef(repo *RepoRecord, ref string) string {
	return repo.Data.Refs[ref]
}

// GetRefOk returns the ref's oid and whether it exists.
func (s *Store) GetRefOk(repo *RepoRecord, ref string) (string, bool) {
	oid, ok := repo.Data.Refs[ref]
	return oid, ok
}

func (s *Store) HasRef(repo *RepoRecord, ref string) bool {
	_, ok := repo.Data.Refs[ref]
	return ok
}

// ListRefs returns [ref, oid] pairs whose name starts with prefix, sorted by ref name.
func (s *Store) ListRefs(repo *RepoRecord, prefix string) [][2]string {
	out := make([][2]string, 0, len(repo.Data.Refs))
	for ref, oid := range repo.Data.Refs {
		if strings.HasPrefix(ref, prefix) {
			out = append(out, [2]string{ref, oid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func isHexPrefix(name string) bool {
	if len(name) < 4 || len(name) > 40 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ResolveRef turns a short ref name into an oid.
func (s *Store) ResolveRef(repo *RepoRecord, short string) (string, bool) {
	name := strings.TrimSpace(short)
	if name == "" {
		return "", false
	}
	if name == "HEAD" {
		oid, ok := repo.Data.Refs["refs/heads/"+repo.DefaultBranch]
		return oid, ok
	}
	var candidates []string
	if strings.HasPrefix(name, "refs/") {
		candidates = []string{name}
	} else {
		candidates = []string{"refs/heads/" + name, "refs/tags/" + name, "refs/" + name, name}
	}
	for _, c := range candidates {
		if oid, ok := repo.Data.Refs[c]; ok {
			return oid, true
		}
	}
	if isHexPrefix(name) {
		hits := []string{}
		for oid := range repo.Data.Objects {
			if strings.HasPrefix(oid, name) {
				hits = append(hits, oid)
			}
		}
		if len(hits) == 1 {
			return hits[0], true
		}
	}
	return "", false
}

// ResolveObject resolves a full or abbreviated oid to a stored object.
func (s *Store) ResolveObject(repo *RepoRecord, short string) (string, *GitObject, bool) {
	if obj, ok := repo.Data.Objects[short]; ok {
		return short, obj, true
	}
	if !isHexPrefix(short) {
		return "", nil, false
	}
	hits := []string{}
	for oid := range repo.Data.Objects {
		if strings.HasPrefix(oid, short) {
			hits = append(hits, oid)
		}
	}
	if len(hits) != 1 {
		return "", nil, false
	}
	obj, ok := repo.Data.Objects[hits[0]]
	if !ok {
		return "", nil, false
	}
	return hits[0], obj, true
}

// CreateLabel creates (or returns the existing) label by name.
func (s *Store) CreateLabel(repo *RepoRecord, name, color string, description *string) *LabelRecord {
	if existing, ok := repo.Data.Labels[name]; ok {
		return existing
	}
	if color == "" {
		color = "ededed"
	}
	id := repo.Data.NextLabel
	repo.Data.NextLabel++
	label := &LabelRecord{
		ID:          id,
		NodeID:      nodeID("Label", id),
		Name:        name,
		Color:       color,
		Description: description,
		URL:         s.Base + "/api/v3/repos/" + repo.FullName + "/labels/" + name,
	}
	repo.Data.Labels[name] = label
	return label
}

func (s *Store) GetLabel(repo *RepoRecord, name string) *LabelRecord {
	return repo.Data.Labels[name]
}

func (s *Store) ListLabels(repo *RepoRecord) []*LabelRecord {
	out := make([]*LabelRecord, 0, len(repo.Data.Labels))
	for _, label := range repo.Data.Labels {
		out = append(out, label)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) LabelsFor(repo *RepoRecord, names []string) []*LabelRecord {
	out := make([]*LabelRecord, 0, len(names))
	for _, name := range names {
		out = append(out, s.CreateLabel(repo, name, "ededed", nil))
	}
	return out
}

// CreateIssue opens an issue on the repo.
func (s *Store) CreateIssue(repo *RepoRecord, title, body, user, state string, labels []string, withLabels bool) *IssueRecord {
	s.Writes++
	number := repo.Data.NextIssue
	repo.Data.NextIssue++
	var labelRecords []*LabelRecord
	if withLabels {
		labelRecords = s.LabelsFor(repo, labels)
	} else {
		labelRecords = []*LabelRecord{}
	}
	if state == "" {
		state = "open"
	}
	issue := &IssueRecord{
		ID:                number + (repo.ID-1)*100000,
		NodeID:            nodeID("Issue", number),
		Number:            number,
		Title:             title,
		Body:              body,
		State:             state,
		Labels:            labelRecords,
		User:              s.CreateUser(user),
		CreatedAt:         EPOCH,
		UpdatedAt:         EPOCH,
		AuthorAssociation: "OWNER",
		HTMLURL:           s.Base + "/" + repo.FullName + "/issues/" + strconv.Itoa(number),
		URL:               s.Base + "/api/v3/repos/" + repo.FullName + "/issues/" + strconv.Itoa(number),
		RepositoryURL:     s.Base + "/api/v3/repos/" + repo.FullName,
		CommentsURL:       s.Base + "/api/v3/repos/" + repo.FullName + "/issues/" + strconv.Itoa(number) + "/comments",
		EventsURL:         s.Base + "/api/v3/repos/" + repo.FullName + "/issues/" + strconv.Itoa(number) + "/events",
		LabelsURL:         s.Base + "/api/v3/repos/" + repo.FullName + "/issues/" + strconv.Itoa(number) + "/labels{/name}",
		TimelineURL:       s.Base + "/api/v3/repos/" + repo.FullName + "/issues/" + strconv.Itoa(number) + "/timeline",
	}
	if state == "closed" {
		closed := EPOCH
		issue.ClosedAt = &closed
	}
	repo.Data.Issues[number] = issue
	if issue.State == "open" {
		repo.OpenIssuesCount++
	}
	return issue
}

func (s *Store) GetIssue(repo *RepoRecord, number int) *IssueRecord {
	return repo.Data.Issues[number]
}

// ListIssues returns the repo's issues filtered by state and labels, by number.
func (s *Store) ListIssues(repo *RepoRecord, state string, labels []string) []*IssueRecord {
	if state == "" {
		state = "open"
	}
	out := []*IssueRecord{}
	for _, issue := range repo.Data.Issues {
		if state != "all" && issue.State != state {
			continue
		}
		ok := true
		for _, want := range labels {
			found := false
			for _, label := range issue.Labels {
				if label.Name == want {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, issue)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// UpdateIssuePatch is the mutable slice of an issue.
type UpdateIssuePatch struct {
	Title       *string
	Body        *string
	State       *string
	Labels      []string
	HasLabels   bool
	StateReason *string
}

func (s *Store) UpdateIssue(repo *RepoRecord, number int, patch UpdateIssuePatch) *IssueRecord {
	s.Writes++
	issue, ok := repo.Data.Issues[number]
	if !ok {
		return nil
	}
	wasOpen := issue.State == "open"
	if patch.Title != nil {
		issue.Title = *patch.Title
	}
	if patch.Body != nil {
		issue.Body = *patch.Body
	}
	if patch.HasLabels {
		issue.Labels = s.LabelsFor(repo, patch.Labels)
	}
	if patch.StateReason != nil {
		issue.StateReason = patch.StateReason
	}
	if patch.State != nil && *patch.State != issue.State {
		issue.State = *patch.State
		if *patch.State == "closed" {
			closed := EPOCH
			issue.ClosedAt = &closed
		} else {
			issue.ClosedAt = nil
		}
		if *patch.State == "closed" && wasOpen {
			if repo.OpenIssuesCount > 0 {
				repo.OpenIssuesCount--
			}
		}
		if *patch.State == "open" && !wasOpen {
			repo.OpenIssuesCount++
		}
	}
	issue.UpdatedAt = EPOCH
	return issue
}

// CreatePull opens a pull request.
func (s *Store) CreatePull(repo *RepoRecord, title, body, user, headRef, headSha, baseRef, baseSha string) *PullRecord {
	s.Writes++
	number := repo.Data.NextPull
	repo.Data.NextPull++
	u := s.CreateUser(user)
	if baseRef == "" {
		baseRef = repo.DefaultBranch
	}
	if baseSha == "" {
		if tip, ok := s.ResolveRef(repo, "refs/heads/"+repo.DefaultBranch); ok {
			baseSha = tip
		} else {
			baseSha = headSha
		}
	}
	mergeable := true
	pull := &PullRecord{
		ID:                  number + (repo.ID-1)*100000,
		NodeID:              nodeID("PullRequest", number),
		Number:              number,
		Title:               title,
		Body:                body,
		State:               "open",
		Mergeable:           &mergeable,
		MergeableState:      "clean",
		Rebaseable:          true,
		User:                u,
		Head:                PullRef{Ref: headRef, Sha: headSha, Label: user + ":" + headRef, User: u},
		Base:                PullRef{Ref: baseRef, Sha: baseSha, Label: repo.OwnerLogin + ":" + baseRef, User: u},
		CreatedAt:           EPOCH,
		UpdatedAt:           EPOCH,
		Commits:             1,
		MaintainerCanModify: true,
		AuthorAssociation:   "OWNER",
		URL:                 s.Base + "/api/v3/repos/" + repo.FullName + "/pulls/" + strconv.Itoa(number),
		HTMLURL:             s.Base + "/" + repo.FullName + "/pull/" + strconv.Itoa(number),
		DiffURL:             s.Base + "/" + repo.FullName + "/pull/" + strconv.Itoa(number) + ".diff",
		PatchURL:            s.Base + "/" + repo.FullName + "/pull/" + strconv.Itoa(number) + ".patch",
		IssueURL:            s.Base + "/api/v3/repos/" + repo.FullName + "/issues/" + strconv.Itoa(number),
		CommentsURL:         s.Base + "/api/v3/repos/" + repo.FullName + "/issues/" + strconv.Itoa(number) + "/comments",
		StatusesURL:         s.Base + "/api/v3/repos/" + repo.FullName + "/statuses/" + headSha,
		CommitsURL:          s.Base + "/api/v3/repos/" + repo.FullName + "/pulls/" + strconv.Itoa(number) + "/commits",
		ReviewCommentsURL:   s.Base + "/api/v3/repos/" + repo.FullName + "/pulls/" + strconv.Itoa(number) + "/comments",
		RepositoryURL:       s.Base + "/api/v3/repos/" + repo.FullName,
	}
	repo.Data.Pulls[number] = pull
	repo.OpenPullsCount++
	return pull
}

func (s *Store) GetPull(repo *RepoRecord, number int) *PullRecord {
	return repo.Data.Pulls[number]
}

// ListPulls returns the repo's pulls filtered by state, by number.
func (s *Store) ListPulls(repo *RepoRecord, state string) []*PullRecord {
	if state == "" {
		state = "open"
	}
	out := []*PullRecord{}
	for _, pull := range repo.Data.Pulls {
		if state != "all" && pull.State != state {
			continue
		}
		out = append(out, pull)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// MergePull closes a pull and records its merge commit.
func (s *Store) MergePull(repo *RepoRecord, number int, mergeSha string) *PullRecord {
	s.Writes++
	pull, ok := repo.Data.Pulls[number]
	if !ok {
		return nil
	}
	if !pull.Merged && pull.State == "open" && repo.OpenPullsCount > 0 {
		repo.OpenPullsCount--
	}
	mergedAt := EPOCH
	closedAt := EPOCH
	pull.Merged = true
	pull.MergedAt = &mergedAt
	pull.MergeCommitSha = &mergeSha
	pull.State = "closed"
	pull.ClosedAt = &closedAt
	pull.UpdatedAt = EPOCH
	pull.Mergeable = nil
	pull.MergeableState = "unknown"
	return pull
}

// UpdatePullPatch is the mutable slice of a pull request.
type UpdatePullPatch struct {
	Title *string
	Body  *string
	State *string
	Base  *string
}

func (s *Store) UpdatePull(repo *RepoRecord, number int, patch UpdatePullPatch) *PullRecord {
	pull, ok := repo.Data.Pulls[number]
	if !ok {
		return nil
	}
	if patch.Title != nil {
		pull.Title = *patch.Title
	}
	if patch.Body != nil {
		pull.Body = *patch.Body
	}
	if patch.Base != nil {
		pull.Base.Ref = *patch.Base
	}
	if patch.State != nil && *patch.State != pull.State {
		wasOpen := pull.State == "open"
		pull.State = *patch.State
		if *patch.State == "closed" {
			closed := EPOCH
			pull.ClosedAt = &closed
		} else {
			pull.ClosedAt = nil
		}
		if *patch.State == "closed" && wasOpen {
			if repo.OpenPullsCount > 0 {
				repo.OpenPullsCount--
			}
		}
		if *patch.State == "open" && !wasOpen {
			repo.OpenPullsCount++
		}
	}
	pull.UpdatedAt = EPOCH
	return pull
}

// CreateComment adds a comment to an issue or a pull (shared numbering).
func (s *Store) CreateComment(repo *RepoRecord, issueNumber int, body, user string) *CommentRecord {
	s.Writes++
	issue, hasIssue := repo.Data.Issues[issueNumber]
	pull, hasPull := repo.Data.Pulls[issueNumber]
	if !hasIssue && !hasPull {
		return nil
	}
	id := repo.Data.NextComment
	repo.Data.NextComment++
	comment := &CommentRecord{
		ID:                id + (repo.ID-1)*100000,
		NodeID:            nodeID("IssueComment", id),
		Body:              body,
		User:              s.CreateUser(user),
		CreatedAt:         EPOCH,
		UpdatedAt:         EPOCH,
		HTMLURL:           s.Base + "/" + repo.FullName + "/issues/" + strconv.Itoa(issueNumber) + "#issuecomment-" + strconv.Itoa(id),
		URL:               s.Base + "/api/v3/repos/" + repo.FullName + "/issues/comments/" + strconv.Itoa(id),
		IssueURL:          s.Base + "/api/v3/repos/" + repo.FullName + "/issues/" + strconv.Itoa(issueNumber),
		AuthorAssociation: "OWNER",
	}
	repo.Data.Comments[id] = comment
	if hasIssue {
		issue.Comments++
	}
	if hasPull {
		pull.Comments++
	}
	return comment
}

func (s *Store) GetComment(repo *RepoRecord, id int) *CommentRecord {
	return repo.Data.Comments[id]
}

// ListComments returns the repo's comments, by id, optionally filtered to one issue.
func (s *Store) ListComments(repo *RepoRecord, issueNumber int, filterByIssue bool) []*CommentRecord {
	out := []*CommentRecord{}
	suffix := "/repos/" + repo.FullName + "/issues/" + strconv.Itoa(issueNumber)
	for _, comment := range repo.Data.Comments {
		if filterByIssue && !strings.HasSuffix(comment.IssueURL, suffix) {
			continue
		}
		out = append(out, comment)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) UpdateComment(repo *RepoRecord, id int, body string) *CommentRecord {
	comment, ok := repo.Data.Comments[id]
	if !ok {
		return nil
	}
	comment.Body = body
	comment.UpdatedAt = EPOCH
	return comment
}

func (s *Store) DeleteComment(repo *RepoRecord, id int) bool {
	if _, ok := repo.Data.Comments[id]; !ok {
		return false
	}
	delete(repo.Data.Comments, id)
	return true
}

// ---------------------------------------------------------------- JSON shapes

func sp(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func bp(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func userJSON(u *UserRecord) map[string]any {
	return map[string]any{
		"login":       u.Login,
		"id":          u.ID,
		"node_id":     u.NodeID,
		"avatar_url":  u.AvatarURL,
		"gravatar_id": "",
		"url":         u.URL,
		"html_url":    u.HTMLURL,
		"type":        u.Type,
		"site_admin":  false,
		"name":        u.Name,
		"email":       u.Email,
	}
}

func repoJSON(repo *RepoRecord) map[string]any {
	return map[string]any{
		"id":                          repo.ID,
		"node_id":                     repo.NodeID,
		"name":                        repo.Name,
		"full_name":                   repo.FullName,
		"private":                     repo.Private,
		"owner":                       userJSON(repo.Owner),
		"html_url":                    repo.HTMLURL,
		"description":                 sp(repo.Description),
		"fork":                        repo.Fork,
		"url":                         repo.URL,
		"created_at":                  repo.CreatedAt,
		"updated_at":                  repo.UpdatedAt,
		"pushed_at":                   repo.PushedAt,
		"git_url":                     repo.GitURL,
		"ssh_url":                     repo.SSHURL,
		"clone_url":                   repo.CloneURL,
		"size":                        repo.Size,
		"stargazers_count":            0,
		"watchers_count":              0,
		"language":                    nil,
		"has_issues":                  repo.HasIssues,
		"has_projects":                repo.HasProjects,
		"has_downloads":               repo.HasDownloads,
		"has_wiki":                    repo.HasWiki,
		"has_pages":                   repo.HasPages,
		"has_discussions":             repo.HasDiscussions,
		"forks_count":                 0,
		"archived":                    repo.Archived,
		"disabled":                    repo.Disabled,
		"open_issues_count":           repo.OpenIssuesCount,
		"license":                     nil,
		"topics":                      repo.Topics,
		"visibility":                  repo.Visibility,
		"default_branch":              repo.DefaultBranch,
		"permissions":                 map[string]any{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
		"allow_forking":               true,
		"is_template":                 false,
		"web_commit_signoff_required": false,
	}
}

func labelJSON(label *LabelRecord) map[string]any {
	return map[string]any{
		"id":          label.ID,
		"node_id":     label.NodeID,
		"name":        label.Name,
		"color":       label.Color,
		"default":     label.Default,
		"description": sp(label.Description),
		"url":         label.URL,
	}
}

func labelsJSON(labels []*LabelRecord) []any {
	out := make([]any, 0, len(labels))
	for _, label := range labels {
		out = append(out, labelJSON(label))
	}
	return out
}

func issueJSON(issue *IssueRecord) map[string]any {
	return map[string]any{
		"id":                 issue.ID,
		"node_id":            issue.NodeID,
		"number":             issue.Number,
		"title":              issue.Title,
		"body":               issue.Body,
		"state":              issue.State,
		"state_reason":       sp(issue.StateReason),
		"locked":             issue.Locked,
		"labels":             labelsJSON(issue.Labels),
		"user":               userJSON(issue.User),
		"assignee":           nil,
		"assignees":          []any{},
		"milestone":          nil,
		"comments":           issue.Comments,
		"created_at":         issue.CreatedAt,
		"updated_at":         issue.UpdatedAt,
		"closed_at":          sp(issue.ClosedAt),
		"author_association": issue.AuthorAssociation,
		"html_url":           issue.HTMLURL,
		"url":                issue.URL,
		"repository_url":     issue.RepositoryURL,
		"comments_url":       issue.CommentsURL,
		"events_url":         issue.EventsURL,
		"labels_url":         issue.LabelsURL,
		"timeline_url":       issue.TimelineURL,
		"active_lock_reason": nil,
	}
}

func pullRefJSON(ref PullRef) map[string]any {
	return map[string]any{
		"ref":   ref.Ref,
		"sha":   ref.Sha,
		"label": ref.Label,
		"user":  userJSON(ref.User),
		"repo":  nil,
	}
}

func pullJSON(pull *PullRecord) map[string]any {
	return map[string]any{
		"id":                    pull.ID,
		"node_id":               pull.NodeID,
		"number":                pull.Number,
		"title":                 pull.Title,
		"body":                  pull.Body,
		"state":                 pull.State,
		"locked":                pull.Locked,
		"draft":                 pull.Draft,
		"merged":                pull.Merged,
		"merged_at":             sp(pull.MergedAt),
		"merge_commit_sha":      sp(pull.MergeCommitSha),
		"mergeable":             bp(pull.Mergeable),
		"mergeable_state":       pull.MergeableState,
		"rebaseable":            pull.Rebaseable,
		"user":                  userJSON(pull.User),
		"head":                  pullRefJSON(pull.Head),
		"base":                  pullRefJSON(pull.Base),
		"created_at":            pull.CreatedAt,
		"updated_at":            pull.UpdatedAt,
		"closed_at":             sp(pull.ClosedAt),
		"commits":               pull.Commits,
		"additions":             pull.Additions,
		"deletions":             pull.Deletions,
		"changed_files":         pull.ChangedFiles,
		"comments":              pull.Comments,
		"review_comments":       pull.ReviewComments,
		"maintainer_can_modify": pull.MaintainerCanModify,
		"author_association":    pull.AuthorAssociation,
		"url":                   pull.URL,
		"html_url":              pull.HTMLURL,
		"diff_url":              pull.DiffURL,
		"patch_url":             pull.PatchURL,
		"issue_url":             pull.IssueURL,
		"comments_url":          pull.CommentsURL,
		"statuses_url":          pull.StatusesURL,
		"commits_url":           pull.CommitsURL,
		"review_comments_url":   pull.ReviewCommentsURL,
		"repository_url":        pull.RepositoryURL,
	}
}

func commentJSON(comment *CommentRecord) map[string]any {
	return map[string]any{
		"id":                 comment.ID,
		"node_id":            comment.NodeID,
		"body":               comment.Body,
		"user":               userJSON(comment.User),
		"created_at":         comment.CreatedAt,
		"updated_at":         comment.UpdatedAt,
		"html_url":           comment.HTMLURL,
		"url":                comment.URL,
		"issue_url":          comment.IssueURL,
		"author_association": comment.AuthorAssociation,
	}
}
