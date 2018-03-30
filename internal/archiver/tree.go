package archiver

import (
	"fmt"
	"path/filepath"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
)

// Tree recursively defines how a snapshot should look like when
// archived.
//
// When `Path` is set, this is a leaf node and the contents of `Path` should be
// inserted at this point in the tree.
//
// The attribute `Root` is used to distinguish between files/dirs which have
// the same name, but live in a separate directory on the local file system.
//
// `FileInfoPath` is used to extract metadata for intermediate (=non-leaf)
// trees.
type Tree struct {
	Nodes        map[string]Tree
	Path         string // where the files/dirs to be saved are found
	FileInfoPath string // where the dir can be found that is not included itself, but its subdirs
	Root         string // parent directory of the tree
}

// pathComponents returns all path components of p. If a virtual directory
// (volume name on Windows) is added, virtualPrefix is set to true. See the
// tests for examples.
func pathComponents(p string, includeRelative bool) (components []string, virtualPrefix bool) {
	volume := filepath.VolumeName(p)

	if !filepath.IsAbs(p) {
		if !includeRelative {
			p = filepath.Join(string(filepath.Separator), p)
		}
	}

	p = filepath.Clean(p)

	for {
		dir, file := filepath.Dir(p), filepath.Base(p)

		if p == dir {
			break
		}

		components = append(components, file)
		p = dir
	}

	// reverse components
	for i := len(components)/2 - 1; i >= 0; i-- {
		opp := len(components) - 1 - i
		components[i], components[opp] = components[opp], components[i]
	}

	if volume != "" {
		// strip colon
		if len(volume) == 2 && volume[1] == ':' {
			volume = volume[:1]
		}

		components = append([]string{volume}, components...)
		virtualPrefix = true
	}

	return components, virtualPrefix
}

// rootDirectory returns the directory which contains the first element of target.
func rootDirectory(target string) string {
	if target == "" {
		return ""
	}

	if filepath.IsAbs(target) {
		return filepath.Join(filepath.VolumeName(target), string(filepath.Separator))
	}

	target = filepath.Clean(target)
	pc, _ := pathComponents(target, true)

	rel := "."
	for _, c := range pc {
		if c == ".." {
			rel = filepath.Join(rel, c)
		}
	}

	return rel
}

// Add adds a new file or directory to the tree.
func (t *Tree) Add(path string) error {
	if path == "" {
		panic("invalid path (empty string)")
	}

	if t.Nodes == nil {
		t.Nodes = make(map[string]Tree)
	}

	pc, virtualPrefix := pathComponents(path, false)
	if len(pc) == 0 {
		return errors.New("invalid path (no path components)")
	}

	name := pc[0]
	root := rootDirectory(path)
	tree := Tree{Root: root}

	origName := name
	i := 0
	for {
		other, ok := t.Nodes[name]
		if !ok {
			break
		}

		i++
		if other.Root == root {
			tree = other
			break
		}

		// resolve conflict and try again
		name = fmt.Sprintf("%s-%d", origName, i)
		continue
	}

	if len(pc) > 1 {
		subroot := filepath.Join(root, origName)
		if virtualPrefix {
			// use the original root dir if this is a virtual directory (volume name on Windows)
			subroot = root
		}
		err := tree.add(path, subroot, pc[1:])
		if err != nil {
			return err
		}
		tree.FileInfoPath = subroot
	} else {
		tree.Path = path
	}

	t.Nodes[name] = tree
	return nil
}

// add adds a new target path into the tree.
func (t *Tree) add(target, root string, pc []string) error {
	if len(pc) == 0 {
		return errors.Errorf("invalid path %q", target)
	}

	if t.Nodes == nil {
		t.Nodes = make(map[string]Tree)
	}

	name := pc[0]

	if len(pc) == 1 {
		tree, ok := t.Nodes[name]

		if !ok {
			t.Nodes[name] = Tree{Path: target}
			return nil
		}

		if tree.Path != "" {
			return errors.Errorf("path is already set for target %v", target)
		}
		tree.Path = target
		t.Nodes[name] = tree
		return nil
	}

	tree := Tree{}
	if other, ok := t.Nodes[name]; ok {
		tree = other
	}

	subroot := filepath.Join(root, name)
	tree.FileInfoPath = subroot

	err := tree.add(target, subroot, pc[1:])
	if err != nil {
		return err
	}
	t.Nodes[name] = tree

	return nil
}

func (t Tree) String() string {
	return formatTree(t, "")
}

// formatTree returns a text representation of the tree t.
func formatTree(t Tree, indent string) (s string) {
	for name, node := range t.Nodes {
		if node.Path != "" {
			s += fmt.Sprintf("%v/%v, src %q\n", indent, name, node.Path)
			continue
		}
		s += fmt.Sprintf("%v/%v, root %q, meta  %q\n", indent, name, node.Root, node.FileInfoPath)
		s += formatTree(node, indent+"    ")
	}
	return s
}

// prune removes sub-trees of leaf nodes.
func prune(t *Tree) {
	// if the current tree is a leaf node (Path is set), remove all nodes,
	// those are automatically included anyway.
	if t.Path != "" && len(t.Nodes) > 0 {
		t.FileInfoPath = ""
		t.Nodes = nil
		return
	}

	for i, subtree := range t.Nodes {
		prune(&subtree)
		t.Nodes[i] = subtree
	}
}

// NewTree creates a Tree from the target files/directories.
func NewTree(targets []string) (*Tree, error) {
	debug.Log("targets: %v", targets)
	tree := &Tree{}
	seen := make(map[string]struct{})
	for _, target := range targets {
		target = filepath.Clean(target)

		// skip duplicate targets
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}

		err := tree.Add(target)
		if err != nil {
			return nil, err
		}
	}

	prune(tree)
	debug.Log("result:\n%v", tree)
	return tree, nil
}
