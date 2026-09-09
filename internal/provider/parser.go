package provider

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Command is an API operation. Path elements are never passed through a shell.
type Command struct {
	Op   string   `json:"op"`
	Path []string `json:"path"`
}

// tokenize implements shell-style quoting, but performs no shell expansion.
// Single quotes are literal. Double quotes preserve backslashes except before
// a quote, backslash, dollar sign, or backtick. Adjacent quoted segments join.
func tokenize(input string) ([]string, error) {
	if !utf8.ValidString(input) {
		return nil, fmt.Errorf("commands must be valid UTF-8")
	}
	var result []string
	var word strings.Builder
	var quote rune
	started := false
	runes := []rune(input)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c == 0 {
			return nil, fmt.Errorf("NUL characters are not supported")
		}
		if c == '\\' && quote != '\'' {
			if i+1 == len(runes) {
				return nil, fmt.Errorf("unfinished escape")
			}
			next := runes[i+1]
			if next == 0 {
				return nil, fmt.Errorf("NUL characters are not supported")
			}
			if quote == '"' && !strings.ContainsRune("\"\\$`", next) {
				word.WriteRune(c)
			} else {
				i++
				word.WriteRune(next)
			}
			started = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				word.WriteRune(c)
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
			started = true
		case unicode.IsSpace(c):
			if started {
				result = append(result, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if started {
		result = append(result, word.String())
	}
	return result, nil
}

func ParseCommand(input string) (Command, error) {
	tokens, err := tokenize(input)
	if err != nil {
		return Command{}, err
	}
	if len(tokens) < 2 {
		return Command{}, fmt.Errorf("a set or delete command must include a nonempty path")
	}
	if tokens[0] != "set" && tokens[0] != "delete" {
		return Command{}, fmt.Errorf("only set and delete commands are supported")
	}
	for i, token := range tokens[1:] {
		if token == "" && i != len(tokens)-2 {
			return Command{}, fmt.Errorf("an empty token is only valid as the last value")
		}
	}
	if tokens[1] == "" {
		return Command{}, fmt.Errorf("the root path must not be empty")
	}
	return Command{Op: tokens[0], Path: tokens[1:]}, nil
}

func ParseCommands(inputs []string) ([]Command, error) {
	var result []Command
	seen := make(map[string]bool)
	for i, input := range inputs {
		c, err := ParseCommand(input)
		if err != nil {
			return nil, fmt.Errorf("command %d: %w", i+1, err)
		}
		key := c.Op + pathKey(c.Path)
		if !seen[key] {
			result = append(result, c)
			seen[key] = true
		}
	}
	sortCommands(result)
	for _, d := range result {
		if d.Op != "delete" {
			continue
		}
		for _, s := range result {
			if s.Op == "set" && hasPrefix(s.Path, d.Path) {
				return nil, fmt.Errorf("a delete assertion contains a desired set path; remove the conflicting commands")
			}
		}
	}
	return result, nil
}

func pathKey(path []string) string { b, _ := json.Marshal(path); return string(b) }
func hasPrefix(path, parent []string) bool {
	return len(path) >= len(parent) && slices.Equal(path[:len(parent)], parent)
}
func child(path []string, name string) []string { return append(slices.Clone(path), name) }
func sortCommands(commands []Command) {
	slices.SortFunc(commands, func(a, b Command) int {
		if a.Op != b.Op {
			return strings.Compare(a.Op, b.Op)
		} // deletes before sets
		return slices.Compare(a.Path, b.Path)
	})
}

func formatPath(path []string) string {
	parts := make([]string, len(path))
	for i, token := range path {
		if token != "" && !strings.ContainsFunc(token, unicode.IsSpace) && !strings.ContainsAny(token, "'\"\\$`;") {
			parts[i] = token
		} else {
			parts[i] = "'" + strings.ReplaceAll(token, "'", "'\\''") + "'"
		}
	}
	return strings.Join(parts, " ")
}
