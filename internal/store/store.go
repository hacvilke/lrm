// Package store manages the on-disk LRM repository layout:
//
//	<root>/.lrm/
//	  config.json        — repo config (user, port, identity dir)
//	  identity.key       — Ed25519 seed (0600)
//	  index.json         — virtual staging index
//	  objects/           — CAS (blobs, trees, commits, chunks)
//	  refs/heads/        — branch tips (hex hash per file)
//	  HEAD               — current branch ref (e.g. "ref: refs/heads/main")
//	  logs/replication.log — replication log (JSONL)
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lrm-project/lrm/internal/cas"
	"github.com/lrm-project/lrm/internal/dag"
	"github.com/lrm-project/lrm/internal/identity"
	"github.com/lrm-project/lrm/internal/replog"
	"github.com/lrm-project/lrm/internal/staging"
	"github.com/lrm-project/lrm/internal/vectorclock"
)

// Config is the repo configuration.
type Config struct {
	Version     int    `json:"version"`
	User        string `json:"user"`
	Port        int    `json:"port"`
	DefaultHead string `json:"default_head"`
}

// DefaultPort is the default LRM WAN/LAN listen port.
const DefaultPort = 8443

// Repo is an opened LRM repository.
type Repo struct {
	Root     string
	LrmDir   string
	Config   Config
	Identity *identity.Identity
	CAS      *cas.Store
	DAG      *dag.Store
	Index    *staging.Index
	Replog   *replog.Log
}

// Init creates a new LRM repo at root (root must exist or be created).
func Init(root, user string, port int) (*Repo, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	lrmDir := filepath.Join(root, ".lrm")
	if _, err := os.Stat(lrmDir); err == nil {
		return nil, fmt.Errorf("%s is already an LRM repo", root)
	}
	for _, d := range []string{
		lrmDir,
		filepath.Join(lrmDir, "objects"),
		filepath.Join(lrmDir, "refs", "heads"),
		filepath.Join(lrmDir, "logs"),
		filepath.Join(lrmDir, "tmp"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if user == "" {
		user = defaultUser()
	}
	if port == 0 {
		port = DefaultPort
	}
	cfg := Config{Version: 1, User: user, Port: port, DefaultHead: "main"}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(lrmDir, "config.json"), raw, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(lrmDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(lrmDir)
	if err != nil {
		return nil, err
	}
	return Open(root, id)
}

// Open opens an existing repo by walking up from startDir to find .lrm.
func Open(startDir string, _ ...any) (*Repo, error) {
	root, err := findRoot(startDir)
	if err != nil {
		return nil, err
	}
	lrmDir := filepath.Join(root, ".lrm")
	raw, err := os.ReadFile(filepath.Join(lrmDir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(lrmDir)
	if err != nil {
		return nil, err
	}
	casStore, err := cas.New(filepath.Join(lrmDir, "objects"))
	if err != nil {
		return nil, err
	}
	idx, err := staging.Load(filepath.Join(lrmDir, "index.json"))
	if err != nil {
		return nil, err
	}
	rl, err := replog.Open(filepath.Join(lrmDir, "logs", "replication.log"))
	if err != nil {
		return nil, err
	}
	return &Repo{
		Root: root, LrmDir: lrmDir, Config: cfg,
		Identity: id, CAS: casStore, DAG: dag.New(casStore),
		Index: idx, Replog: rl,
	}, nil
}

// Close releases repo resources.
func (r *Repo) Close() error {
	var first error
	if err := r.Index.Save(); err != nil && first == nil {
		first = err
	}
	if err := r.Replog.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

func findRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, ".lrm")); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("not an LRM repo (no .lrm found walking up from %s)", start)
		}
		abs = parent
	}
}

func defaultUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "lrm-user"
}

// --- refs ---

// HeadBranch returns the current branch name (e.g. "main").
func (r *Repo) HeadBranch() (string, error) {
	raw, err := os.ReadFile(filepath.Join(r.LrmDir, "HEAD"))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "ref: ") {
		ref := strings.TrimPrefix(s, "ref: ")
		return strings.TrimPrefix(ref, "refs/heads/"), nil
	}
	return "main", nil
}

// SetHeadBranch switches HEAD to a branch.
func (r *Repo) SetHeadBranch(branch string) error {
	return os.WriteFile(filepath.Join(r.LrmDir, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644)
}

// GetRef reads a branch tip (empty string if unborn).
func (r *Repo) GetRef(branch string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(r.LrmDir, "refs", "heads", branch))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// SetRef writes a branch tip.
func (r *Repo) SetRef(branch, hashHex string) error {
	return os.WriteFile(filepath.Join(r.LrmDir, "refs", "heads", branch), []byte(hashHex+"\n"), 0o644)
}

// ListBranches returns all branch names.
func (r *Repo) ListBranches() ([]string, error) {
	dir := filepath.Join(r.LrmDir, "refs", "heads")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// --- tags (lightweight: name -> commit hash) ---

// validTagName rejects empty names and path tricks.
func validTagName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return false
	}
	return true
}

// CreateTag points name at targetHex (must be an existing commit).
func (r *Repo) CreateTag(name, targetHex string) error {
	if !validTagName(name) {
		return fmt.Errorf("invalid tag name %q", name)
	}
	h, err := cas.ParseHex(targetHex)
	if err != nil {
		return fmt.Errorf("bad target %q: %w", targetHex, err)
	}
	if !r.DAG.Has(h) {
		return fmt.Errorf("target %q is not a known commit", targetHex)
	}
	dir := filepath.Join(r.LrmDir, "refs", "tags")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("tag %q already exists", name)
	}
	return os.WriteFile(p, []byte(targetHex+"\n"), 0o644)
}

// DeleteTag removes a tag.
func (r *Repo) DeleteTag(name string) error {
	if !validTagName(name) {
		return fmt.Errorf("invalid tag name %q", name)
	}
	p := filepath.Join(r.LrmDir, "refs", "tags", name)
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no such tag %q", name)
		}
		return err
	}
	return nil
}

// GetTag returns the target hash hex of a tag ("" if missing).
func (r *Repo) GetTag(name string) (string, error) {
	if !validTagName(name) {
		return "", nil
	}
	raw, err := os.ReadFile(filepath.Join(r.LrmDir, "refs", "tags", name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// ListTags returns name -> target-hex for all tags.
func (r *Repo) ListTags() (map[string]string, error) {
	out := map[string]string{}
	ents, err := os.ReadDir(filepath.Join(r.LrmDir, "refs", "tags"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r.LrmDir, "refs", "tags", e.Name()))
		if err != nil {
			continue
		}
		out[e.Name()] = strings.TrimSpace(string(raw))
	}
	return out, nil
}

// HeadCommit returns the current branch tip hash (Nil if unborn).
func (r *Repo) HeadCommit() (cas.Hash, string, error) {
	br, err := r.HeadBranch()
	if err != nil {
		return cas.Nil, "", err
	}
	hexStr, err := r.GetRef(br)
	if err != nil {
		return cas.Nil, br, err
	}
	if hexStr == "" {
		return cas.Nil, br, nil
	}
	h, err := cas.ParseHex(hexStr)
	return h, br, err
}

// --- commits ---

// Commit creates a commit on the current branch from the current Merkle root.
func (r *Repo) Commit(message string, rootHash cas.Hash) (cas.Hash, *dag.Commit, error) {
	br, err := r.HeadBranch()
	if err != nil {
		return cas.Nil, nil, err
	}
	tipHex, _ := r.GetRef(br)
	var parents []string
	clock := vectorclock.Clock{}
	if tipHex != "" {
		parents = append(parents, tipHex)
		tipH, err := cas.ParseHex(tipHex)
		if err != nil {
			return cas.Nil, nil, err
		}
		tipC, err := r.DAG.Get(tipH)
		if err != nil {
			return cas.Nil, nil, err
		}
		clock = tipC.Clock.Clone()
	}
	clock.Increment(r.Identity.HexID())
	c := &dag.Commit{
		Version: 1, Tree: cas.Hex(rootHash), Parents: parents,
		Author: r.Config.User, PeerHex: r.Identity.HexID(),
		Timestamp: time.Now().UnixNano(), Message: message, Clock: clock,
	}
	h, err := r.DAG.Put(c)
	if err != nil {
		return cas.Nil, nil, err
	}
	if err := r.SetRef(br, cas.Hex(h)); err != nil {
		return cas.Nil, nil, err
	}
	r.AppendReflog(br, tipHex, cas.Hex(h), "commit", message)
	_, _ = r.Replog.Append(replog.Entry{
		Type: replog.TypeCommit, PeerHex: r.Identity.HexID(),
		Commit: cas.Hex(h), Message: message, Clock: clock.Clone(),
		Extra: map[string]string{"branch": br},
	})
	return h, c, nil
}
