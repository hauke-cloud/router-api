package command

// Op is what a Change does.
type Op string

const (
	// OpSet creates a node or sets a value.
	OpSet Op = "set"
	// OpDelete removes a node and everything below it.
	OpDelete Op = "delete"
)

// Change is one step from the current configuration to the desired one.
type Change struct {
	Op   Op
	Path Path
}

// String renders the change as a VyOS command.
func (c Change) String() string {
	return string(c.Op) + " " + c.Path.join()
}

// Diff returns the changes that turn current into desired: every delete
// first, then the sets in the order desired lists them. Deleting first is
// what makes changing a value work, since the old value is a delete and the
// new one a set of the same node.
//
// Nothing below a path in keep is deleted. That is for configuration the
// router has to retain although nobody listed it.
func Diff(current, desired, keep []Path) []Change {
	// Every ancestor of a desired command, itself included: a node on this
	// list still has something below it that is wanted.
	wanted := map[string]struct{}{}
	for _, path := range desired {
		for depth := 1; depth <= len(path); depth++ {
			wanted[path[:depth].key()] = struct{}{}
		}
	}

	var changes []Change
	deleted := map[string]struct{}{}
	for _, path := range current {
		if _, ok := wanted[path.key()]; ok || kept(path, keep) {
			continue
		}
		// Delete at the highest node nothing wanted lives under. Removing
		// only the leaf would leave its parents behind as empty nodes.
		root := path
		for depth := 1; depth <= len(path); depth++ {
			if _, ok := wanted[path[:depth].key()]; !ok {
				root = path[:depth]
				break
			}
		}
		if _, done := deleted[root.key()]; done {
			continue
		}
		deleted[root.key()] = struct{}{}
		changes = append(changes, Change{Op: OpDelete, Path: root})
	}

	currentKeys := make(map[string]struct{}, len(current))
	for _, path := range current {
		currentKeys[path.key()] = struct{}{}
	}
	for _, path := range desired {
		if _, ok := currentKeys[path.key()]; !ok {
			changes = append(changes, Change{Op: OpSet, Path: path})
		}
	}
	return changes
}

func kept(path Path, keep []Path) bool {
	for _, prefix := range keep {
		if path.HasPrefix(prefix) {
			return true
		}
	}
	return false
}
