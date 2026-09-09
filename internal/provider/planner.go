package provider

import "fmt"

type DeleteResolution struct {
	Source []string
	Path   []string
	Reason string
}
type Batch struct {
	Operations  []Command
	Resolutions []DeleteResolution
}

// BuildBatch is pure. State owns exact SET assertions, never all descendants of
// a parent assertion. Explicit DELETEs separately authorize their stated subtree.
func BuildBatch(s *Snapshot, owned, desired []Command) (Batch, error) {
	var batch Batch
	wanted := map[string]bool{}
	var sets, assertions []Command
	for _, c := range desired {
		if c.Op == "set" {
			if n := s.nodes[pathKey(c.Path)]; n != nil && len(*n.Data.Values) > 0 {
				return batch, fmt.Errorf("a desired SET ends at an active value node without specifying its value")
			}
			sets = append(sets, c)
			wanted[pathKey(c.Path)] = true
		} else {
			assertions = append(assertions, c)
		}
	}
	// Count removable running terminals at every prefix. A value changed outside
	// Terraform is not the old owned value, so removal never adopts it implicitly.
	removed := map[string]bool{}
	counts := map[string]int{}
	for _, c := range owned {
		key := pathKey(c.Path)
		if c.Op != "set" || wanted[key] {
			continue
		}
		covered := false
		for _, assertion := range assertions {
			if hasPrefix(c.Path, assertion.Path) {
				covered = true
				break
			}
		}
		// Explicit assertions authorize their precise scope, including comments.
		// Keep them out of automatic pruning so that scope cannot grow.
		if covered {
			continue
		}
		if _, present := s.terminals[key]; present {
			removed[key] = true
		}
	}
	for key := range removed {
		path := s.terminals[key]
		for i := 1; i <= len(path); i++ {
			counts[pathKey(path[:i])]++
		}
	}
	allRemoved := func(path []string) bool {
		key := pathKey(path)
		return counts[key] > 0 && counts[key] == s.totals[key] && !s.hasCommentBelow(path)
	}
	// Ancestors required by a desired SET must survive pruning. The value node
	// itself can still be deleted for a scalar replacement and then set again.
	wantedBelow := map[string]bool{}
	for _, c := range sets {
		for i := 1; i <= len(c.Path); i++ {
			wantedBelow[pathKey(c.Path[:i])] = true
		}
	}
	var deletes []Command
	for key := range removed {
		source := s.terminals[key]
		path := source
		reason := "exact owned value or empty node exists"
		if _, node := s.nodes[key]; !node && len(source) > 1 {
			parent := source[:len(source)-1]
			if allRemoved(parent) {
				path = parent
				reason = "all active values of this node are being removed"
			} else if s.hasCommentBelow(parent) && counts[pathKey(parent)] == s.totals[pathKey(parent)] {
				return batch, fmt.Errorf("removal would erase an unmanaged node comment; remove the comment or use an explicit delete assertion")
			}
		}
		if s.hasCommentBelow(path) {
			return batch, fmt.Errorf("removal would erase an unmanaged node comment")
		}
		for len(path) > 1 {
			parent := path[:len(path)-1]
			if wantedBelow[pathKey(parent)] || !allRemoved(parent) {
				break
			}
			path = parent
			reason = "all active descendants are owned and being removed"
		}
		deletes = append(deletes, Command{Op: "delete", Path: path})
		batch.Resolutions = append(batch.Resolutions, DeleteResolution{Source: source, Path: path, Reason: reason})
	}
	for _, c := range assertions {
		if s.Exists(c.Path) {
			deletes = append(deletes, c)
		}
	}
	deletes = minimizeDeletes(deletes)
	batch.Operations = append(batch.Operations, deletes...)
	for _, c := range sets {
		needed := !s.Exists(c.Path)
		for _, d := range deletes {
			if hasPrefix(c.Path, d.Path) {
				needed = true
			}
		}
		if needed {
			batch.Operations = append(batch.Operations, c)
		}
	}
	sortCommands(batch.Operations)
	return batch, nil
}

func minimizeDeletes(commands []Command) []Command {
	sortCommands(commands)
	var result []Command
	// Lexical token order puts each ancestor immediately before its descendants.
	for _, c := range commands {
		if len(result) > 0 && hasPrefix(c.Path, result[len(result)-1].Path) {
			continue
		}
		result = append(result, c)
	}
	return result
}
