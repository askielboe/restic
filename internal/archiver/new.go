package archiver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/restic/chunker"
	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/restic"
)

// SelectFunc returns true for all items that should be included (files and
// dirs). If false is returned, files are ignored and dirs are not even walked.
type SelectFunc func(item string, fi os.FileInfo) bool

// ErrorFunc is called when an error during archiving occurs. When nil is
// returned, the archiver continues, otherwise it aborts and passes the error
// up the call stack.
type ErrorFunc func(file string, fi os.FileInfo, err error) error

// ReportFunc is called for all files in the backup.
type ReportFunc func(item string, fi os.FileInfo, action ReportAction)

// ReportAction describes what the archiver decided to do with a new file.
type ReportAction int

// These constants are the possible report actions
const (
	ReportActionUnknown   = 0
	ReportActionNew       = iota // New file, will be archived as is
	ReportActionUnchanged = iota // File is unchanged, the old content from the previous snapshot is used
	ReportActionModified  = iota // File has been modified, data will be re-read
)

func (a ReportAction) String() string {
	switch a {
	case ReportActionNew:
		return "new"
	case ReportActionUnchanged:
		return "unchanged"
	case ReportActionModified:
		return "modified"
	default:
		return fmt.Sprintf("unknown (%d)", a)
	}
}

// Str returns a single character for each action.
func (a ReportAction) Str() string {
	switch a {
	case ReportActionNew:
		return "+"
	case ReportActionUnchanged:
		return " "
	case ReportActionModified:
		return "m"
	default:
		return "U"
	}
}

// NewArchiver saves a directory structure to the repo.
type NewArchiver struct {
	Repo   restic.Repository
	Select SelectFunc
	FS     fs.FS

	Report ReportFunc
	Error  ErrorFunc

	WithAtime bool

	m          sync.Mutex
	knownBlobs restic.BlobSet
}

// NewNewArchiver initializes a new archiver.
func NewNewArchiver(repo restic.Repository, fs fs.FS) *NewArchiver {
	return &NewArchiver{
		Repo:   repo,
		Select: func(string, os.FileInfo) bool { return true },
		FS:     fs,

		knownBlobs: make(restic.BlobSet),
	}
}

// Valid returns an error if anything is missing.
func (arch *NewArchiver) Valid() error {
	if arch.knownBlobs == nil {
		return errors.New("known blobs is nil")
	}

	if arch.Repo == nil {
		return errors.New("repo is not set")
	}

	if arch.Select == nil {
		return errors.New("Select is not set")
	}

	if arch.FS == nil {
		return errors.New("FS is not set")
	}

	return nil
}

// report calls arch.Report if it is set.
func (arch *NewArchiver) report(item string, fi os.FileInfo, action ReportAction) {
	if arch.Report == nil {
		return
	}

	arch.Report(item, fi, action)
}

// error calls arch.Error if it is set.
func (arch *NewArchiver) error(item string, fi os.FileInfo, err error) error {
	if arch.Error == nil {
		return err
	}

	errf := arch.Error(item, fi, err)
	if err != errf {
		debug.Log("item %v: error was filtered by handler, before: %q, after: %v", item, err, errf)
	}
	return errf
}

// saveBlob stores a blob in the repo. It checks the index and the known blobs
// before saving anything.
func (arch *NewArchiver) saveBlob(ctx context.Context, t restic.BlobType, buf []byte) (restic.ID, error) {
	id := restic.Hash(buf)
	if arch.Repo.Index().Has(id, t) {
		return id, nil
	}

	h := restic.BlobHandle{ID: id, Type: t}

	// check if another goroutine has already saved this blob
	known := false
	arch.m.Lock()
	if arch.knownBlobs.Has(h) {
		known = true
	} else {
		arch.knownBlobs.Insert(h)
		known = false
	}
	arch.m.Unlock()

	if known {
		return id, nil
	}

	_, err := arch.Repo.SaveBlob(ctx, t, buf, id)
	return id, err
}

// saveTree stores a tree in the repo. It checks the index and the known blobs
// before saving anything.
func (arch *NewArchiver) saveTree(ctx context.Context, t *restic.Tree) (restic.ID, error) {
	buf, err := json.Marshal(t)
	if err != nil {
		return restic.ID{}, errors.Wrap(err, "MarshalJSON")
	}

	// append a newline so that the data is always consistent (json.Encoder
	// adds a newline after each object)
	buf = append(buf, '\n')

	return arch.saveBlob(ctx, restic.TreeBlob, buf)
}

// nodeFromFileInfo returns the restic node from a os.FileInfo.
func (arch *NewArchiver) nodeFromFileInfo(filename string, fi os.FileInfo) (*restic.Node, error) {
	node, err := restic.NodeFromFileInfo(filename, fi)
	if !arch.WithAtime {
		node.AccessTime = node.ModTime
	}
	return node, err
}

// SaveFile chunks a file and saves it to the repository.
func (arch *NewArchiver) SaveFile(ctx context.Context, filename string) (*restic.Node, error) {
	debug.Log("%v", filename)
	f, err := arch.FS.OpenFile(filename, fs.O_RDONLY|fs.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}

	chnker := chunker.New(f, arch.Repo.Config().ChunkerPolynomial)

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, "Stat")
	}

	node, err := arch.nodeFromFileInfo(f.Name(), fi)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	if node.Type != "file" {
		_ = f.Close()
		return nil, errors.Errorf("node type %q is wrong", node.Type)
	}

	node.Content = []restic.ID{}
	buf := make([]byte, chunker.MinSize)
	for {
		chunk, err := chnker.Next(buf)
		if errors.Cause(err) == io.EOF {
			break
		}
		if err != nil {
			_ = f.Close()
			return nil, err
		}

		// test if the context has been cancelled, return the error
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, ctx.Err()
		}

		id, err := arch.saveBlob(ctx, restic.DataBlob, chunk.Data)
		if err != nil {
			_ = f.Close()
			return nil, err
		}

		// test if the context has been cancelled, return the error
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, ctx.Err()
		}

		node.Content = append(node.Content, id)
		buf = chunk.Data
	}

	err = f.Close()
	if err != nil {
		return nil, err
	}

	return node, nil
}

// loadSubtree tries to load the subtree referenced by node. In case of an error, nil is returned.
func (arch *NewArchiver) loadSubtree(ctx context.Context, node *restic.Node) *restic.Tree {
	if node == nil || node.Type != "dir" || node.Subtree == nil {
		return nil
	}

	tree, err := arch.Repo.LoadTree(ctx, *node.Subtree)
	if err != nil {
		debug.Log("unable to load tree %v: %v", node.Subtree.Str(), err)
		// TODO: handle error
		return nil
	}

	return tree
}

// SaveDir stores a directory in the repo and returns the node.
func (arch *NewArchiver) SaveDir(ctx context.Context, prefix string, fi os.FileInfo, dir string, previous *restic.Tree) (*restic.Node, error) {
	debug.Log("%v %v", prefix, dir)

	treeNode, err := arch.nodeFromFileInfo(dir, fi)
	if err != nil {
		return nil, err
	}

	entries, err := readdir(arch.FS, dir)
	if err != nil {
		return nil, err
	}

	tree := restic.NewTree()
	for _, fi := range entries {
		pathname := filepath.Join(dir, fi.Name())
		oldNode := previous.Find(fi.Name())
		node, err := arch.Save(ctx, prefix, pathname, oldNode)
		if err != nil {
			return nil, err
		}

		// Save returns a nil node if the target is excluded
		if node == nil {
			continue
		}

		err = tree.Insert(node)
		if err != nil {
			return nil, err
		}
	}

	id, err := arch.saveTree(ctx, tree)
	if err != nil {
		return nil, err
	}

	treeNode.Subtree = &id
	return treeNode, nil
}

// SnapshotOptions bundle attributes for a new snapshot.
type SnapshotOptions struct {
	Hostname string
	Time     time.Time
	Tags     []string
	Parent   restic.ID
	Targets  []string
}

// Save saves a target (file or directory) to the repo. When an error occurs,
// arch.error() is called to handle it. If the callback ignores the error, or
// the item is excluded, this function returns a nil node and error.
func (arch *NewArchiver) Save(ctx context.Context, prefix, target string, previous *restic.Node) (node *restic.Node, err error) {
	debug.Log("%v target %q, previous %v", prefix, target, previous)
	fi, err := arch.FS.Lstat(target)
	if err != nil {
		return nil, err
	}

	abstarget, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}

	if !arch.Select(abstarget, fi) {
		debug.Log("%v is excluded", target)
		return nil, nil
	}

	switch {
	case fs.IsRegularFile(fi):
		// use previous node if the file hasn't changed
		if previous != nil && !previous.IsNewer(target, fi) {
			debug.Log("%v hasn't changed, returning old node", target)
			arch.report(target, fi, ReportActionUnchanged)
			return previous, err
		}

		if previous != nil {
			arch.report(target, fi, ReportActionModified)
		} else {
			arch.report(target, fi, ReportActionNew)
		}
		node, err = arch.SaveFile(ctx, target)
	case fi.IsDir():
		oldSubtree := arch.loadSubtree(ctx, previous)
		node, err = arch.SaveDir(ctx, prefix, fi, target, oldSubtree)
	default:
		node, err = arch.nodeFromFileInfo(target, fi)
	}

	if err != nil {
		// make sure the node is nil when the callback decided to ignore the
		// error
		return nil, arch.error(abstarget, fi, err)
	}

	return node, err
}

// fileChanged returns true if the file's content has changed since the node
// was created.
func fileChanged(fi os.FileInfo, node *restic.Node) bool {
	if node == nil {
		return true
	}

	// check type change
	if node.Type != "file" {
		return true
	}

	// check modification timestamp
	if !fi.ModTime().Equal(node.ModTime) {
		return true
	}

	// check size
	extFI := fs.ExtendedStat(fi)
	if uint64(fi.Size()) != node.Size || uint64(extFI.Size) != node.Size {
		return true
	}

	// check inode
	if node.Inode != extFI.Inode {
		return true
	}

	return false
}

// SaveTree stores a Tree in the repo, returned is the tree.
func (arch *NewArchiver) SaveTree(ctx context.Context, prefix string, atree *Tree, previous *restic.Tree) (*restic.Tree, error) {
	debug.Log("%v (%v nodes), parent %v", prefix, len(atree.Nodes), previous)

	tree := restic.NewTree()

	for name, subatree := range atree.Nodes {
		debug.Log("%v save node %v", prefix, name)

		// this is a leaf node
		if subatree.Path != "" {
			node, err := arch.Save(ctx, path.Join(prefix, name), subatree.Path, previous.Find(name))
			if err != nil {
				return nil, err
			}

			if node == nil {
				debug.Log("%v excluded: %v", prefix, name)
				continue
			}

			node.Name = name

			err = tree.Insert(node)
			if err != nil {
				return nil, err
			}

			continue
		}

		oldSubtree := arch.loadSubtree(ctx, previous.Find(name))

		// not a leaf node, archive subtree
		subtree, err := arch.SaveTree(ctx, path.Join(prefix, name), &subatree, oldSubtree)
		if err != nil {
			return nil, err
		}

		id, err := arch.saveTree(ctx, subtree)
		if err != nil {
			return nil, err
		}

		if subatree.FileInfoPath == "" {
			return nil, errors.Errorf("FileInfoPath for %v/%v is empty", prefix, name)
		}

		debug.Log("%v, saved subtree %v as %v", prefix, subtree, id.Str())

		fi, err := arch.FS.Lstat(subatree.FileInfoPath)
		if err != nil {
			return nil, err
		}

		debug.Log("%v, dir node data loaded from %v", prefix, subatree.FileInfoPath)

		node, err := arch.nodeFromFileInfo(subatree.FileInfoPath, fi)
		if err != nil {
			return nil, err
		}

		node.Name = name
		node.Subtree = &id

		err = tree.Insert(node)
		if err != nil {
			return nil, err
		}
	}

	return tree, nil
}

func readdir(fs fs.FS, dir string) ([]os.FileInfo, error) {
	f, err := fs.Open(dir)
	if err != nil {
		return nil, err
	}

	entries, err := f.Readdir(-1)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	err = f.Close()
	if err != nil {
		return nil, err
	}

	return entries, nil
}

func readdirnames(fs fs.FS, dir string) ([]string, error) {
	f, err := fs.Open(dir)
	if err != nil {
		return nil, err
	}

	entries, err := f.Readdirnames(-1)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	err = f.Close()
	if err != nil {
		return nil, err
	}

	return entries, nil
}

// resolveRelativeTargets replaces targets that only contain relative
// directories ("." or "../../") to the contents of the directory.
func resolveRelativeTargets(fs fs.FS, targets []string) ([]string, error) {
	result := make([]string, 0, len(targets))
	for _, target := range targets {
		pc, _ := pathComponents(target, false)
		if len(pc) > 0 {
			result = append(result, target)
			continue
		}

		debug.Log("replacing %q with readdir(%q)", target, target)
		entries, err := readdirnames(fs, target)
		if err != nil {
			return nil, err
		}

		for _, name := range entries {
			result = append(result, filepath.Join(target, name))
		}
	}

	return result, nil
}

// Options collect attributes for a new snapshot.
type Options struct {
	Tags           []string
	Hostname       string
	Excludes       []string
	Time           time.Time
	ParentSnapshot restic.ID
}

// loadParentTree loads a tree referenced by snapshot id. If id is null, nil is returned.
func (arch *NewArchiver) loadParentTree(ctx context.Context, snapshotID restic.ID) *restic.Tree {
	if snapshotID.IsNull() {
		return nil
	}

	debug.Log("load parent snapshot %v", snapshotID)
	sn, err := restic.LoadSnapshot(ctx, arch.Repo, snapshotID)
	if err != nil {
		debug.Log("unable to load snapshot %v: %v", snapshotID, err)
		return nil
	}

	if sn.Tree == nil {
		debug.Log("snapshot %v has empty tree %v", snapshotID)
		return nil
	}

	debug.Log("load parent tree %v", *sn.Tree)
	tree, err := arch.Repo.LoadTree(ctx, *sn.Tree)
	if err != nil {
		debug.Log("unable to load tree %v: %v", *sn.Tree, err)
		return nil
	}
	return tree
}

// Snapshot saves several targets and returns a snapshot.
func (arch *NewArchiver) Snapshot(ctx context.Context, targets []string, opts Options) (*restic.Snapshot, restic.ID, error) {
	err := arch.Valid()
	if err != nil {
		return nil, restic.ID{}, err
	}

	var cleanTargets []string
	for _, t := range targets {
		cleanTargets = append(cleanTargets, filepath.Clean(t))
	}

	debug.Log("targets before resolving: %v", cleanTargets)

	cleanTargets, err = resolveRelativeTargets(arch.FS, cleanTargets)
	if err != nil {
		return nil, restic.ID{}, err
	}

	debug.Log("targets after resolving: %v", cleanTargets)

	atree, err := NewTree(cleanTargets)
	if err != nil {
		return nil, restic.ID{}, err
	}

	tree, err := arch.SaveTree(ctx, "/", atree, arch.loadParentTree(ctx, opts.ParentSnapshot))
	if err != nil {
		return nil, restic.ID{}, err
	}

	rootTreeID, err := arch.saveTree(ctx, tree)
	if err != nil {
		return nil, restic.ID{}, err
	}

	err = arch.Repo.Flush(ctx)
	if err != nil {
		return nil, restic.ID{}, err
	}

	err = arch.Repo.SaveIndex(ctx)
	if err != nil {
		return nil, restic.ID{}, err
	}

	sn, err := restic.NewSnapshot(targets, opts.Tags, opts.Hostname, opts.Time)
	sn.Excludes = opts.Excludes
	if !opts.ParentSnapshot.IsNull() {
		id := opts.ParentSnapshot
		sn.Parent = &id
	}
	sn.Tree = &rootTreeID

	id, err := arch.Repo.SaveJSONUnpacked(ctx, restic.SnapshotFile, sn)
	if err != nil {
		return nil, restic.ID{}, err
	}

	return sn, id, nil
}
