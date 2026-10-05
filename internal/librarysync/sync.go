// Package librarysync keeps a local working copy of a TJUClaw library, the
// way a git checkout mirrors a repository: clone writes the library as files,
// status and diff compare local edits with the last sync, pull brings remote
// changes in and push sends local ones back.
//
// Layout: folders are directories; a note is "<title>.md" (and a directory of
// the same name when it has child notes); a file keeps its name and bytes.
// Rich-text notes are exported as read-only Markdown. Sync state lives in
// .tjuclaw/ at the root. Every change sent carries the version it was based
// on, so a change made elsewhere is reported as a conflict, never overwritten.
package librarysync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yunzaixi-dev/tjucli/internal/account"
)

const (
	StateDir      = ".tjuclaw"
	stateFile     = "state.json"
	baseDir       = "base"      // last-synced text of notes, for diff and merges
	conflictDir   = "conflicts" // remote versions that clashed with local edits
	maxNoteRunes  = 200000      // the library's note length; longer Markdown is kept as a file
	maxFileBytes  = 8 << 20
	KindNote      = "note"
	KindRichText  = "rich_text"
	KindFile      = "file"
	KindFolder    = "folder"
	stateVersion  = 1
	untitledNote  = "未命名笔记"
	untitledFile  = "未命名文件"
	untitledGroup = "未命名"
)

var ErrNotWorkingCopy = errors.New("not_a_working_copy")

// Entry is a library entry as the API returns it.
type Entry struct {
	ID          string `json:"id"`
	LibraryID   string `json:"library_id"`
	ParentID    string `json:"parent_id"`
	SortOrder   int    `json:"sort_order"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	UpdatedAt   string `json:"updated_at"`
}

type Library struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Tracked is what the working copy knows about one synced path.
type Tracked struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	UpdatedAt string `json:"updated_at"`
	SHA       string `json:"sha256,omitempty"`
	// Container marks a directory that holds a note's children; the note
	// itself is tracked at "<dir>.md".
	Container bool `json:"container,omitempty"`
	// ConflictAt is the remote version saved under .tjuclaw/conflicts.
	ConflictAt string `json:"conflict_updated_at,omitempty"`
}

type State struct {
	Version     int                `json:"version"`
	API         string             `json:"api"`
	LibraryID   string             `json:"library_id"`
	LibraryName string             `json:"library_name"`
	Entries     map[string]Tracked `json:"entries"`
}

// Copy is an opened working copy.
type Copy struct {
	Root   string
	State  State
	client *account.Client
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// segment makes a title safe as one path component on every desktop system.
func segment(title, fallback string) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r < 32:
			b.WriteRune('_')
		case strings.ContainsRune(`\/:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), ". ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	if s == "" {
		return fallback
	}
	return s
}

// noteTitle is the title for a local note file name.
func noteTitle(name string) string {
	return strings.TrimSuffix(name, ".md")
}

func isNoteFile(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".md") }

// Find opens the working copy containing dir (or above it).
func Find(dir string, client *account.Client) (*Copy, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		data, err := os.ReadFile(filepath.Join(abs, StateDir, stateFile))
		if err == nil {
			var st State
			if json.Unmarshal(data, &st) != nil || st.Version != stateVersion || st.LibraryID == "" {
				return nil, errors.New("working_copy_state_invalid")
			}
			if st.Entries == nil {
				st.Entries = map[string]Tracked{}
			}
			st.API = account.CanonicalAPI(st.API)
			if client != nil && st.API != "" && st.API != client.API {
				return nil, errors.New("working_copy_api_mismatch")
			}
			return &Copy{Root: abs, State: st, client: client}, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return nil, ErrNotWorkingCopy
		}
		abs = parent
	}
}

func (c *Copy) save() error {
	if err := os.MkdirAll(filepath.Join(c.Root, StateDir), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.State, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(c.Root, StateDir, stateFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(c.Root, StateDir, stateFile))
}

func (c *Copy) abs(rel string) string { return filepath.Join(c.Root, filepath.FromSlash(rel)) }

func (c *Copy) basePath(id string) string { return filepath.Join(c.Root, StateDir, baseDir, id) }

func (c *Copy) writeBase(id string, data []byte) error {
	if err := os.MkdirAll(filepath.Join(c.Root, StateDir, baseDir), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.basePath(id), data, 0o600)
}

// Base is the last-synced text of a note, for diff.
func (c *Copy) Base(id string) ([]byte, error) { return os.ReadFile(c.basePath(id)) }

// --- remote ------------------------------------------------------------------

func ListLibraries(ctx context.Context, client *account.Client) ([]Library, error) {
	var out struct {
		Libraries []Library `json:"libraries"`
	}
	if err := client.Do(ctx, "GET", "/libraries", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Libraries, nil
}

// ResolveLibrary finds a library by id or exact name.
func ResolveLibrary(ctx context.Context, client *account.Client, nameOrID string) (Library, error) {
	libs, err := ListLibraries(ctx, client)
	if err != nil {
		return Library{}, err
	}
	var match []Library
	for _, lib := range libs {
		if lib.ID == nameOrID {
			return lib, nil
		}
		if lib.Name == nameOrID {
			match = append(match, lib)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return Library{}, errors.New("library_not_found")
	default:
		return Library{}, errors.New("library_name_ambiguous")
	}
}

func (c *Copy) remoteEntries(ctx context.Context) ([]Entry, error) {
	var out struct {
		Entries []Entry `json:"entries"`
	}
	if err := c.client.Do(ctx, "GET", "/libraries/"+url.PathEscape(c.State.LibraryID)+"/entries", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

func (c *Copy) remoteEntry(ctx context.Context, id string) (Entry, error) {
	var out struct {
		Entry Entry `json:"entry"`
	}
	err := c.client.Do(ctx, "GET", "/entries/"+url.PathEscape(id), nil, nil, &out)
	return out.Entry, err
}

func (c *Copy) remoteContent(ctx context.Context, e Entry) ([]byte, error) {
	switch e.Kind {
	case KindFile:
		var data []byte
		err := c.client.Do(ctx, "GET", "/entries/"+url.PathEscape(e.ID)+"/file", nil, nil, &data)
		return data, err
	case KindNote, KindRichText:
		full, err := c.remoteEntry(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		if full.Kind == KindRichText {
			return []byte(RichTextToMarkdown(full.Body)), nil
		}
		return []byte(full.Body), nil
	}
	return nil, nil
}

// --- layout ------------------------------------------------------------------

// layout maps every remote entry the working copy shows to its local path.
// Kinds other than folders, notes and files appear only as directories when
// they hold children.
type layout struct {
	paths  map[string]string // entry id -> slash path
	byPath map[string]Entry
	dirs   map[string]string // directory path -> entry id that is its parent
}

func buildLayout(entries []Entry) layout {
	byID := map[string]Entry{}
	children := map[string][]Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, e := range entries {
		parent := e.ParentID
		if _, ok := byID[parent]; !ok {
			parent = ""
		}
		children[parent] = append(children[parent], e)
	}
	l := layout{paths: map[string]string{}, byPath: map[string]Entry{}, dirs: map[string]string{"": ""}}
	var walk func(parentID, dir string, seen map[string]bool)
	walk = func(parentID, dir string, seen map[string]bool) {
		kids := children[parentID]
		sort.SliceStable(kids, func(i, j int) bool {
			if kids[i].SortOrder != kids[j].SortOrder {
				return kids[i].SortOrder < kids[j].SortOrder
			}
			return kids[i].ID < kids[j].ID
		})
		taken := map[string]bool{}
		unique := func(name string) string {
			stem, ext := name, ""
			if i := strings.LastIndex(name, "."); i > 0 {
				stem, ext = name[:i], name[i:]
			}
			candidate := name
			for n := 2; taken[strings.ToLower(candidate)]; n++ {
				candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
			}
			taken[strings.ToLower(candidate)] = true
			return candidate
		}
		for _, e := range kids {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			hasKids := len(children[e.ID]) > 0
			switch e.Kind {
			case KindFolder:
				p := path.Join(dir, unique(segment(e.Title, untitledGroup)))
				l.paths[e.ID], l.byPath[p], l.dirs[p] = p, e, e.ID
				walk(e.ID, p, seen)
			case KindNote, KindRichText:
				name := segment(noteTitle(e.Title), untitledNote)
				name = strings.TrimSuffix(unique(name+".md"), ".md")
				p := path.Join(dir, name+".md")
				l.paths[e.ID], l.byPath[p] = p, e
				if hasKids {
					d := path.Join(dir, name)
					taken[strings.ToLower(name)] = true
					l.dirs[d] = e.ID
					walk(e.ID, d, seen)
				}
			case KindFile:
				p := path.Join(dir, unique(segment(e.Title, untitledFile)))
				l.paths[e.ID], l.byPath[p] = p, e
			default:
				if hasKids {
					d := path.Join(dir, unique(segment(e.Title, untitledGroup)))
					l.dirs[d] = e.ID
					walk(e.ID, d, seen)
				}
			}
		}
	}
	walk("", "", map[string]bool{})
	return l
}

// --- local scan --------------------------------------------------------------

type localFile struct {
	path string
	sha  string
	size int64
	long bool // Markdown that does not fit in a note
}

// scan lists local files and directories, skipping .tjuclaw and any hidden
// name (dotfiles are never library content).
func (c *Copy) scan() (map[string]localFile, map[string]bool, error) {
	files, dirs := map[string]localFile{}, map[string]bool{}
	err := filepath.WalkDir(c.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(c.Root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs[rel] = true
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[rel] = localFile{path: rel, sha: digest(data), size: int64(len(data)), long: isNoteFile(rel) && !noteFits(data)}
		return nil
	})
	return files, dirs, err
}

// --- status ------------------------------------------------------------------

type Change struct {
	Op   string `json:"op"` // added, modified, deleted, moved
	Path string `json:"path"`
	From string `json:"from,omitempty"`
	Kind string `json:"kind"`
	Note string `json:"note,omitempty"` // why it cannot be pushed, when it cannot
}

// Status compares the working copy with the last sync.
func (c *Copy) Status() ([]Change, error) {
	files, dirs, err := c.scan()
	if err != nil {
		return nil, err
	}
	var changes []Change
	deleted := map[string]Tracked{}
	for p, t := range c.State.Entries {
		if t.Kind == KindFolder || t.Container {
			if !dirs[p] && t.Kind == KindFolder {
				changes = append(changes, Change{Op: "deleted", Path: p, Kind: KindFolder})
			}
			continue
		}
		f, ok := files[p]
		switch {
		case !ok:
			deleted[p] = t
		case f.sha != t.SHA:
			ch := Change{Op: "modified", Path: p, Kind: t.Kind}
			if t.Kind == KindRichText {
				ch.Note = "rich_text_read_only"
			}
			if t.ConflictAt != "" {
				ch.Note = "conflict"
			}
			changes = append(changes, ch)
		case t.ConflictAt != "":
			changes = append(changes, Change{Op: "modified", Path: p, Kind: t.Kind, Note: "conflict"})
		}
	}
	// A deleted path whose exact content reappears elsewhere was moved.
	bySHA := map[string]string{}
	for p, t := range deleted {
		bySHA[t.SHA] = p
	}
	var added []string
	for p := range files {
		if _, ok := c.State.Entries[p]; !ok {
			added = append(added, p)
		}
	}
	sort.Strings(added)
	for _, p := range added {
		kind := KindFile
		if isNoteFile(p) {
			kind = KindNote
		}
		if from, ok := bySHA[files[p].sha]; ok && (deleted[from].Kind == KindFile) == (kind == KindFile) {
			delete(bySHA, files[p].sha)
			t := deleted[from]
			delete(deleted, from)
			ch := Change{Op: "moved", Path: p, From: from, Kind: t.Kind}
			if t.Kind == KindRichText {
				ch.Note = "rich_text_read_only"
			}
			changes = append(changes, ch)
			continue
		}
		if kind == KindNote && files[p].long {
			kind = KindFile // too long for a note: kept as a Markdown file
		}
		ch := Change{Op: "added", Path: p, Kind: kind}
		if kind == KindFile && files[p].size > maxFileBytes {
			ch.Note = "too_large"
		}
		if files[p].size == 0 && kind == KindFile {
			ch.Note = "empty_file"
		}
		changes = append(changes, ch)
	}
	for p, t := range deleted {
		ch := Change{Op: "deleted", Path: p, Kind: t.Kind}
		if t.Kind == KindRichText {
			ch.Note = "rich_text_read_only"
		}
		changes = append(changes, ch)
	}
	for d := range dirs {
		if _, ok := c.State.Entries[d]; !ok {
			if t, ok := c.State.Entries[d+".md"]; !ok || t.Kind == KindFile {
				changes = append(changes, Change{Op: "added", Path: d, Kind: KindFolder})
			}
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Op < changes[j].Op
	})
	return changes, nil
}

// --- clone and pull ------------------------------------------------------------

// Clone creates a working copy of lib in dir, which must be empty or absent.
func Clone(ctx context.Context, client *account.Client, lib Library, dir string) (*Copy, PullResult, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, PullResult{}, err
	}
	if items, err := os.ReadDir(abs); err == nil && len(items) > 0 {
		return nil, PullResult{}, errors.New("clone_target_not_empty")
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, PullResult{}, err
	}
	c := &Copy{Root: abs, client: client, State: State{
		Version: stateVersion, API: client.API, LibraryID: lib.ID, LibraryName: lib.Name, Entries: map[string]Tracked{},
	}}
	if err := c.save(); err != nil {
		return nil, PullResult{}, err
	}
	res, err := c.Pull(ctx)
	return c, res, err
}

// Init creates a library and makes dir its working copy. dir may already hold
// files: they are new to the library, and the next push sends them.
func Init(ctx context.Context, client *account.Client, name, dir string) (*Copy, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, StateDir, stateFile)); err == nil {
		return nil, errors.New("already_a_working_copy")
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	var out struct {
		Library Library `json:"library"`
	}
	if err := client.Do(ctx, "POST", "/libraries", map[string]string{"name": name}, nil, &out); err != nil {
		return nil, err
	}
	if out.Library.ID == "" {
		return nil, errors.New("api_error")
	}
	c := &Copy{Root: abs, client: client, State: State{
		Version: stateVersion, API: client.API, LibraryID: out.Library.ID, LibraryName: name, Entries: map[string]Tracked{},
	}}
	// The new library's own starter entries come down; the local files stay new.
	if _, err := c.Pull(ctx); err != nil {
		return c, err
	}
	return c, c.save()
}

type PullResult struct {
	Updated   []string `json:"updated"`
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Moved     []string `json:"moved"`
	Conflicts []string `json:"conflicts"`
	Kept      []string `json:"kept"` // deleted remotely but edited here: left untracked
}

func writeFileAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tjuclaw-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func localSHA(p string) (string, bool) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return digest(data), true
}

// Pull brings remote changes into the working copy. A file edited here and
// remotely is left as is; the remote version goes to .tjuclaw/conflicts.
func (c *Copy) Pull(ctx context.Context) (PullResult, error) {
	res := PullResult{Updated: []string{}, Added: []string{}, Removed: []string{}, Moved: []string{}, Conflicts: []string{}, Kept: []string{}}
	entries, err := c.remoteEntries(ctx)
	if err != nil {
		return res, err
	}
	l := buildLayout(entries)
	byID := map[string]string{} // tracked id -> local path
	for p, t := range c.State.Entries {
		if !t.Container {
			byID[t.ID] = p
		}
	}
	next := map[string]Tracked{}
	// Directories first: folders and the note containers.
	var dirPaths []string
	for d, id := range l.dirs {
		if d == "" {
			continue
		}
		dirPaths = append(dirPaths, d)
		e, isFolder := l.byPath[d]
		if isFolder && e.Kind == KindFolder {
			next[d] = Tracked{ID: id, Kind: KindFolder, UpdatedAt: e.UpdatedAt}
		} else {
			next[d] = Tracked{ID: id, Kind: KindFolder, Container: true}
		}
	}
	sort.Strings(dirPaths)
	for _, d := range dirPaths {
		if err := os.MkdirAll(c.abs(d), 0o755); err != nil {
			return res, err
		}
	}
	var paths []string
	for p := range l.byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		e := l.byPath[p]
		if e.Kind == KindFolder {
			continue
		}
		old, tracked := byID[e.ID]
		t := c.State.Entries[old]
		if tracked && t.UpdatedAt == e.UpdatedAt && old == p {
			next[p] = t
			continue
		}
		localChanged := false
		if tracked {
			sha, exists := localSHA(c.abs(old))
			localChanged = !exists || sha != t.SHA
		} else if _, exists := localSHA(c.abs(p)); exists {
			localChanged = true // an untracked local file already sits here
		}
		data, err := c.remoteContent(ctx, e)
		if err != nil {
			return res, err
		}
		sha := digest(data)
		if localChanged {
			if tracked && t.UpdatedAt == e.UpdatedAt {
				// Only the path moved remotely; keep the local edit where it is.
				next[old] = t
				continue
			}
			if err := writeFileAtomic(filepath.Join(c.Root, StateDir, conflictDir, filepath.FromSlash(p)), data); err != nil {
				return res, err
			}
			res.Conflicts = append(res.Conflicts, p)
			if tracked {
				t.ConflictAt = e.UpdatedAt
				next[old] = t
			}
			continue
		}
		if tracked && old != p {
			_ = os.Remove(c.abs(old))
			res.Moved = append(res.Moved, old+" -> "+p)
		} else if tracked {
			res.Updated = append(res.Updated, p)
		} else {
			res.Added = append(res.Added, p)
		}
		if err := writeFileAtomic(c.abs(p), data); err != nil {
			return res, err
		}
		if e.Kind != KindFile {
			if err := c.writeBase(e.ID, data); err != nil {
				return res, err
			}
		}
		next[p] = Tracked{ID: e.ID, Kind: e.Kind, UpdatedAt: e.UpdatedAt, SHA: sha}
	}
	// Tracked entries gone remotely.
	present := map[string]bool{}
	for _, e := range entries {
		present[e.ID] = true
	}
	for p, t := range c.State.Entries {
		if present[t.ID] || t.Container || t.Kind == KindFolder {
			continue
		}
		sha, exists := localSHA(c.abs(p))
		switch {
		case !exists:
		case sha == t.SHA:
			_ = os.Remove(c.abs(p))
			res.Removed = append(res.Removed, p)
		default:
			res.Kept = append(res.Kept, p)
		}
		_ = os.Remove(c.basePath(t.ID))
	}
	// Remote folders that are gone: remove their now-empty directories.
	for p, t := range c.State.Entries {
		if t.Kind == KindFolder && !present[t.ID] && !t.Container {
			if err := os.Remove(c.abs(p)); err == nil {
				res.Removed = append(res.Removed, p)
			}
		}
	}
	c.State.Entries = next
	return res, c.save()
}

// --- push ----------------------------------------------------------------------

type PushResult struct {
	Created   []string  `json:"created"`
	Updated   []string  `json:"updated"`
	Moved     []string  `json:"moved"`
	Deleted   []string  `json:"deleted"`
	Conflicts []string  `json:"conflicts"`
	Skipped   []Change  `json:"skipped"`
	Failed    []Failure `json:"failed"`
}

// Failure is one item the API refused; the rest of the push went on.
type Failure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Progress reports how many of the items being sent are done.
type Progress func(done, total int)

const (
	pushWorkers    = 4
	saveEveryItems = 20
)

func contentType(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return t
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".md":
		return "text/markdown"
	}
	return "application/octet-stream"
}

// parentFor finds the remote parent id for a local path's directory,
// creating folders for directories the library does not have yet.
func (c *Copy) parentFor(ctx context.Context, dir string, res *PushResult) (string, error) {
	if dir == "." || dir == "" {
		return "", nil
	}
	if t, ok := c.State.Entries[dir]; ok && t.Kind == KindFolder {
		return t.ID, nil
	}
	if t, ok := c.State.Entries[dir+".md"]; ok && (t.Kind == KindNote || t.Kind == KindRichText) {
		return t.ID, nil
	}
	parent, err := c.parentFor(ctx, path.Dir(dir), res)
	if err != nil {
		return "", err
	}
	var out struct {
		Folder Entry `json:"folder"`
	}
	body := map[string]any{"title": path.Base(dir)}
	if parent != "" {
		body["parent_id"] = parent
	}
	if err := c.client.Do(ctx, "POST", "/libraries/"+url.PathEscape(c.State.LibraryID)+"/folders", body, nil, &out); err != nil {
		return "", err
	}
	if out.Folder.ID == "" {
		return "", errors.New("api_error")
	}
	c.State.Entries[dir] = Tracked{ID: out.Folder.ID, Kind: KindFolder, UpdatedAt: out.Folder.UpdatedAt}
	res.Created = append(res.Created, dir+"/")
	return out.Folder.ID, nil
}

func validText(data []byte) bool {
	return utf8.Valid(data) && !strings.ContainsRune(string(data), 0)
}

// noteFits reports whether text can be a note: valid UTF-8 within the
// library's note length. Longer Markdown is kept as a file instead.
func noteFits(data []byte) bool {
	return validText(data) && utf8.RuneCount(data) <= maxNoteRunes
}

// fatal is an error that stops the whole push: signed out or canceled.
// Anything else concerns one item, which is reported and skipped.
func fatal(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	var remote *account.RemoteError
	return errors.As(err, &remote) && (remote.Status == 401 || remote.Status == 403)
}

// transient is a failure that says nothing about the item: the edge, the
// network or the server, not the request.
func transient(err error) bool {
	var remote *account.RemoteError
	if errors.As(err, &remote) {
		return remote.Status >= 500 || remote.Status == 429
	}
	return true
}

// maxTransientInARow stops a push once this many items in a row failed for
// reasons that are not theirs: the connection is down, not the items.
const maxTransientInARow = 8

// remoteKey identifies an entry the way a new local path would create it.
func remoteKey(parent, kind, title string) string { return parent + "\x00" + kind + "\x00" + title }

// Push sends local changes. Each change carries the version it was based on;
// a remote change since then is a conflict and that path is left unsent. A
// refused item is reported in Failed and the push goes on. Progress is
// saved as it goes, so an interrupted push resumes without duplicates.
func (c *Copy) Push(ctx context.Context, dryRun bool, progress Progress) (PushResult, error) {
	res := PushResult{Created: []string{}, Updated: []string{}, Moved: []string{}, Deleted: []string{}, Conflicts: []string{}, Skipped: []Change{}, Failed: []Failure{}}
	changes, err := c.Status()
	if err != nil {
		return res, err
	}
	var work []Change
	for _, ch := range changes {
		switch {
		case ch.Note == "conflict":
			res.Conflicts = append(res.Conflicts, ch.Path)
		case ch.Note != "":
			res.Skipped = append(res.Skipped, ch)
		default:
			work = append(work, ch)
		}
	}
	if dryRun {
		return res, nil
	}
	defer func() { _ = c.save() }()
	var mu sync.Mutex // guards res, c.State and the save counter in the upload phase
	done, total, sinceSave, inARow := 0, len(work), 0, 0
	// Entries on the server that this copy does not track yet: an earlier
	// push that lost its reply (a 504 from the edge) may have created them.
	// A new local item that matches one is adopted instead of sent again.
	adoptable := map[string]Entry{}
	if hasAdds(work) {
		remote, err := c.remoteEntries(ctx)
		if err != nil {
			return res, err
		}
		known := map[string]bool{}
		for _, t := range c.State.Entries {
			known[t.ID] = true
		}
		for _, e := range remote {
			if !known[e.ID] && (e.Kind == KindNote || e.Kind == KindFile) {
				adoptable[remoteKey(e.ParentID, e.Kind, e.Title)] = e
			}
		}
	}
	step := func() {
		done++
		if sinceSave++; sinceSave >= saveEveryItems {
			sinceSave = 0
			_ = c.save()
		}
		if progress != nil {
			progress(done, total)
		}
	}
	// handle records an item's error; it returns the error only when fatal.
	handle := func(p string, err error) error {
		if err == nil {
			inARow = 0
			return nil
		}
		if account.IsCode(err, "entry_conflict") {
			res.Conflicts = append(res.Conflicts, p)
			return nil
		}
		if fatal(ctx, err) {
			return fmt.Errorf("%s: %w", p, err)
		}
		if transient(err) {
			if inARow++; inARow >= maxTransientInARow {
				return fmt.Errorf("%s: %w", p, errors.New("api_unreachable"))
			}
		}
		var remote *account.RemoteError
		id := err.Error()
		if errors.As(err, &remote) {
			id = remote.ID
		}
		res.Failed = append(res.Failed, Failure{Path: p, Error: id})
		return nil
	}

	// 1. Folders, parents before children, including empty ones.
	for _, ch := range work {
		if ch.Op == "added" && ch.Kind == KindFolder {
			if _, err := c.parentFor(ctx, ch.Path, &res); err != nil {
				if err := handle(ch.Path+"/", err); err != nil {
					return res, err
				}
			}
			step()
		}
	}
	// 2. Moves and edits, one at a time.
	for _, ch := range work {
		var err error
		switch ch.Op {
		case "moved":
			err = c.pushMove(ctx, ch, &res)
		case "modified":
			err = c.pushEdit(ctx, ch, &res)
		default:
			continue
		}
		if err := handle(ch.Path, err); err != nil {
			return res, err
		}
		step()
	}
	// 3. New notes and files, a few at a time.
	var adds []Change
	for _, ch := range work {
		if ch.Op == "added" && ch.Kind != KindFolder {
			adds = append(adds, ch)
		}
	}
	jobs := make(chan Change)
	var fatalErr error
	var wg sync.WaitGroup
	for range pushWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ch := range jobs {
				mu.Lock()
				parent, perr := c.parentFor(ctx, path.Dir(ch.Path), &res)
				mu.Unlock()
				var tracked Tracked
				var data []byte
				err := perr
				adopted := false
				if err == nil {
					mu.Lock()
					candidate, found := adoptable[remoteKey(parent, ch.Kind, newTitle(ch))]
					delete(adoptable, remoteKey(parent, ch.Kind, newTitle(ch)))
					mu.Unlock()
					if found {
						tracked, data, adopted = c.adopt(ctx, ch, candidate)
					}
					if !adopted {
						tracked, data, err = c.pushNew(ctx, ch, parent)
					}
				}
				mu.Lock()
				if err == nil {
					inARow = 0
					c.State.Entries[ch.Path] = tracked
					if tracked.Kind == KindNote {
						_ = c.writeBase(tracked.ID, data)
					}
					res.Created = append(res.Created, ch.Path)
				} else if ferr := handle(ch.Path, err); ferr != nil && fatalErr == nil {
					fatalErr = ferr
				}
				step()
				mu.Unlock()
			}
		}()
	}
	for _, ch := range adds {
		mu.Lock()
		stop := fatalErr != nil
		mu.Unlock()
		if stop {
			break
		}
		jobs <- ch
	}
	close(jobs)
	wg.Wait()
	if fatalErr != nil {
		return res, fatalErr
	}
	// 4. Deletions last: an entry edited remotely since the last sync is kept.
	for _, ch := range work {
		if ch.Op != "deleted" || ch.Kind == KindFolder {
			continue
		}
		if err := handle(ch.Path, c.pushDelete(ctx, ch, &res)); err != nil {
			return res, err
		}
		step()
	}
	// A folder is deleted only once nothing is left in it remotely, so a
	// removed directory never takes remote-only content with it.
	var folderDeletes []string
	for _, ch := range work {
		if ch.Op == "deleted" && ch.Kind == KindFolder {
			folderDeletes = append(folderDeletes, ch.Path)
		}
	}
	sort.Slice(folderDeletes, func(i, j int) bool { return len(folderDeletes[i]) > len(folderDeletes[j]) })
	for _, p := range folderDeletes {
		entries, err := c.remoteEntries(ctx)
		if err != nil {
			return res, err
		}
		t := c.State.Entries[p]
		empty := true
		for _, e := range entries {
			if e.ParentID == t.ID {
				empty = false
				break
			}
		}
		if !empty {
			res.Skipped = append(res.Skipped, Change{Op: "deleted", Path: p, Kind: KindFolder, Note: "folder_not_empty_remotely"})
			step()
			continue
		}
		if err := c.client.Do(ctx, "DELETE", "/folders/"+url.PathEscape(t.ID), nil, nil, nil); err != nil && !account.IsCode(err, "folder_not_found") {
			if err := handle(p+"/", err); err != nil {
				return res, err
			}
			step()
			continue
		}
		delete(c.State.Entries, p)
		res.Deleted = append(res.Deleted, p+"/")
		step()
	}
	return res, nil
}

func (c *Copy) pushMove(ctx context.Context, ch Change, res *PushResult) error {
	t := c.State.Entries[ch.From]
	parent, err := c.parentFor(ctx, path.Dir(ch.Path), res)
	if err != nil {
		return err
	}
	title := path.Base(ch.Path)
	if t.Kind != KindFile {
		title = noteTitle(title)
	}
	var out struct {
		Entry Entry `json:"entry"`
	}
	if err := c.client.Do(ctx, "PATCH", "/entries/"+url.PathEscape(t.ID), map[string]any{
		"title": title, "parent_id": parent, "expected_updated_at": t.UpdatedAt,
	}, nil, &out); err != nil {
		return err
	}
	delete(c.State.Entries, ch.From)
	t.UpdatedAt = out.Entry.UpdatedAt
	c.State.Entries[ch.Path] = t
	res.Moved = append(res.Moved, ch.From+" -> "+ch.Path)
	return nil
}

func (c *Copy) pushEdit(ctx context.Context, ch Change, res *PushResult) error {
	t := c.State.Entries[ch.Path]
	data, err := os.ReadFile(c.abs(ch.Path))
	if err != nil {
		return err
	}
	var out struct {
		Entry Entry `json:"entry"`
	}
	if t.Kind == KindFile {
		if len(data) == 0 || len(data) > maxFileBytes {
			res.Skipped = append(res.Skipped, Change{Op: ch.Op, Path: ch.Path, Kind: t.Kind, Note: "too_large"})
			return nil
		}
		err = c.client.Do(ctx, "PUT", "/entries/"+url.PathEscape(t.ID)+"/file", data,
			map[string]string{"X-TJUClaw-Expected-Updated-At": t.UpdatedAt, "Content-Type": contentType(ch.Path)}, &out)
	} else {
		if !noteFits(data) {
			res.Skipped = append(res.Skipped, Change{Op: ch.Op, Path: ch.Path, Kind: t.Kind, Note: "too_large"})
			return nil
		}
		err = c.client.Do(ctx, "PATCH", "/entries/"+url.PathEscape(t.ID), map[string]any{
			"body": string(data), "expected_updated_at": t.UpdatedAt,
		}, nil, &out)
	}
	if err != nil {
		return err
	}
	t.UpdatedAt, t.SHA = out.Entry.UpdatedAt, digest(data)
	c.State.Entries[ch.Path] = t
	if t.Kind != KindFile {
		_ = c.writeBase(t.ID, data)
	}
	res.Updated = append(res.Updated, ch.Path)
	return nil
}

func hasAdds(work []Change) bool {
	for _, ch := range work {
		if ch.Op == "added" && ch.Kind != KindFolder {
			return true
		}
	}
	return false
}

// newTitle is the title a new local path gets in the library.
func newTitle(ch Change) string {
	if ch.Kind == KindNote {
		return noteTitle(path.Base(ch.Path))
	}
	return path.Base(ch.Path)
}

// adopt takes an untracked server entry as this local item when it holds the
// same content: a note with the same text, a file of the same size.
func (c *Copy) adopt(ctx context.Context, ch Change, e Entry) (Tracked, []byte, bool) {
	data, err := os.ReadFile(c.abs(ch.Path))
	if err != nil {
		return Tracked{}, nil, false
	}
	switch e.Kind {
	case KindFile:
		if e.Size != len(data) {
			return Tracked{}, nil, false
		}
	case KindNote:
		full, err := c.remoteEntry(ctx, e.ID)
		if err != nil || full.Body != string(data) {
			return Tracked{}, nil, false
		}
	}
	return Tracked{ID: e.ID, Kind: e.Kind, UpdatedAt: e.UpdatedAt, SHA: digest(data)}, data, true
}

// pushNew creates one note or file; it touches no shared state, so several
// run at once.
func (c *Copy) pushNew(ctx context.Context, ch Change, parent string) (Tracked, []byte, error) {
	data, err := os.ReadFile(c.abs(ch.Path))
	if err != nil {
		return Tracked{}, nil, err
	}
	var out struct {
		Entry Entry `json:"entry"`
	}
	if ch.Kind == KindNote {
		body := map[string]any{"kind": KindNote, "title": noteTitle(path.Base(ch.Path)), "body": string(data)}
		if parent != "" {
			body["parent_id"] = parent
		}
		err = c.client.Do(ctx, "POST", "/libraries/"+url.PathEscape(c.State.LibraryID)+"/entries", body, nil, &out)
	} else {
		fields := map[string]string{}
		if parent != "" {
			fields["parent_id"] = parent
		}
		form, ferr := account.NewUpload(path.Base(ch.Path), contentType(ch.Path), data, fields)
		if ferr != nil {
			return Tracked{}, nil, ferr
		}
		err = c.client.Do(ctx, "POST", "/libraries/"+url.PathEscape(c.State.LibraryID)+"/files", form, nil, &out)
	}
	if err != nil {
		return Tracked{}, nil, err
	}
	return Tracked{ID: out.Entry.ID, Kind: out.Entry.Kind, UpdatedAt: out.Entry.UpdatedAt, SHA: digest(data)}, data, nil
}

func (c *Copy) pushDelete(ctx context.Context, ch Change, res *PushResult) error {
	t := c.State.Entries[ch.Path]
	remote, err := c.remoteEntry(ctx, t.ID)
	if account.IsCode(err, "entry_not_found") {
		delete(c.State.Entries, ch.Path)
		return nil
	}
	if err != nil {
		return err
	}
	if remote.UpdatedAt != t.UpdatedAt {
		res.Conflicts = append(res.Conflicts, ch.Path)
		return nil
	}
	if err := c.client.Do(ctx, "DELETE", "/entries/"+url.PathEscape(t.ID), nil, nil, nil); err != nil && !account.IsCode(err, "entry_not_found") {
		return err
	}
	delete(c.State.Entries, ch.Path)
	_ = os.Remove(c.basePath(t.ID))
	res.Deleted = append(res.Deleted, ch.Path)
	return nil
}

// Resolve accepts the local file as the merge of a conflict: the next push
// replaces the remote version recorded when the conflict was found.
func (c *Copy) Resolve(rel string) error {
	rel = path.Clean(filepath.ToSlash(rel))
	t, ok := c.State.Entries[rel]
	if !ok || t.ConflictAt == "" {
		return errors.New("no_conflict")
	}
	t.UpdatedAt, t.ConflictAt = t.ConflictAt, ""
	t.SHA = "" // the local file now counts as modified
	c.State.Entries[rel] = t
	_ = os.Remove(filepath.Join(c.Root, StateDir, conflictDir, filepath.FromSlash(rel)))
	return c.save()
}

// ConflictCopy is where pull saved the remote version of a conflicting path.
func (c *Copy) ConflictCopy(rel string) string {
	return filepath.Join(c.Root, StateDir, conflictDir, filepath.FromSlash(rel))
}
