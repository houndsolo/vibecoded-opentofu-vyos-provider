package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// VyOS json_ast preserves node/value boundaries, empty containers and comments.
// Ordinary showConfig JSON and flat set lines lose some of this information.
type configNode struct {
	Name     *string       `json:"name"`
	Data     *nodeData     `json:"data"`
	Children *[]configNode `json:"children"`
}
type nodeData struct {
	Values  *[]string `json:"values"`
	Comment *string   `json:"comment"`
	Tag     *bool     `json:"tag"`
	Leaf    *bool     `json:"leaf"`
}

type Snapshot struct {
	nodes     map[string]*configNode
	terminals map[string][]string
	totals    map[string]int
	comments  [][]string
}

func DecodeSnapshot(raw []byte) (*Snapshot, error) {
	var root configNode
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// Unknown AST fields may carry new ownership-relevant metadata. Fail closed.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("unsupported VyOS json_ast configuration: %w", err)
	}
	s := &Snapshot{nodes: map[string]*configNode{}, terminals: map[string][]string{}, totals: map[string]int{}}
	var walk func(*configNode, []string) error
	walk = func(n *configNode, path []string) error {
		if n.Name == nil || n.Data == nil || n.Children == nil || n.Data.Values == nil || n.Data.Leaf == nil || n.Data.Tag == nil {
			return fmt.Errorf("incomplete VyOS json_ast node; refusing to infer ownership")
		}
		if len(path) == 0 && *n.Name != "" {
			return fmt.Errorf("expected the full VyOS configuration root")
		}
		if len(*n.Children) > 0 && len(*n.Data.Values) > 0 {
			return fmt.Errorf("configuration node has both children and values")
		}
		s.nodes[pathKey(path)] = n
		if n.Data.Comment != nil {
			s.comments = append(s.comments, path)
		}
		add := func(p []string) {
			key := pathKey(p)
			if _, ok := s.terminals[key]; !ok {
				s.terminals[key] = p
				for i := 1; i <= len(p); i++ {
					s.totals[pathKey(p[:i])]++
				}
			}
		}
		if len(*n.Children) == 0 {
			if len(*n.Data.Values) == 0 && len(path) > 0 {
				add(path)
			}
			for _, value := range *n.Data.Values {
				add(child(path, value))
			}
		}
		seen := map[string]bool{}
		for i := range *n.Children {
			c := &(*n.Children)[i]
			if c.Name == nil || *c.Name == "" || seen[*c.Name] {
				return fmt.Errorf("invalid or duplicate configuration node name")
			}
			seen[*c.Name] = true
			if err := walk(c, child(path, *c.Name)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(&root, nil); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Snapshot) Exists(path []string) bool {
	_, node := s.nodes[pathKey(path)]
	_, value := s.terminals[pathKey(path)]
	return node || value
}

func (s *Snapshot) hasCommentBelow(path []string) bool {
	for _, comment := range s.comments {
		if hasPrefix(comment, path) {
			return true
		}
	}
	return false
}

// VerifyActive requires the session AST and operational active export to agree.
// This prevents a pending shared REST session from being mistaken for running
// configuration. The two reads are not a server-side concurrency lock.
func (s *Snapshot) VerifyActive(text string) error {
	seen := map[string]bool{}
	comments := map[string]string{}
	for _, record := range configurationRecords(text) {
		tokens, err := tokenize(record)
		if err != nil || len(tokens) < 2 {
			return fmt.Errorf("cannot parse active configuration export")
		}
		switch tokens[0] {
		case "set":
			key := pathKey(tokens[1:])
			if _, ok := s.terminals[key]; !ok {
				return fmt.Errorf("REST session and active configuration disagree")
			}
			seen[key] = true
		case "comment":
			if len(tokens) < 3 {
				return fmt.Errorf("invalid active comment export")
			}
			key := pathKey(tokens[1 : len(tokens)-1])
			n := s.nodes[key]
			if n == nil || n.Data.Comment == nil || *n.Data.Comment != tokens[len(tokens)-1] {
				return fmt.Errorf("REST session and active comments disagree")
			}
			comments[key] = tokens[len(tokens)-1]
		default:
			return fmt.Errorf("unsupported active configuration export command")
		}
	}
	if len(seen) != len(s.terminals) {
		return fmt.Errorf("REST session and active configuration disagree")
	}
	for _, path := range s.comments {
		n := s.nodes[pathKey(path)]
		if n.Data.Comment != nil && *n.Data.Comment != "" && comments[pathKey(path)] != *n.Data.Comment {
			return fmt.Errorf("REST session and active comments disagree")
		}
	}
	return nil
}

// Split only at newlines outside quotes. Banners and descriptions may contain
// literal newlines, so splitting by physical lines is unsafe.
func configurationRecords(text string) []string {
	var records []string
	var quote rune
	start := 0
	escaped := false
	for i, c := range text {
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '\n' {
			if record := strings.TrimSpace(text[start:i]); record != "" {
				records = append(records, record)
			}
			start = i + 1
		}
	}
	if record := strings.TrimSpace(text[start:]); record != "" {
		records = append(records, record)
	}
	return records
}
