// Package command reads VyOS configuration in its "set" command form and
// works out how to get from one configuration to another.
package command

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Path is one configuration command without its verb: the node names from the
// root down, ending in a value where the node has one.
type Path []string

// String renders the path as a "set" command that ParseLine reads back.
func (p Path) String() string {
	return "set " + p.join()
}

func (p Path) join() string {
	quoted := make([]string, len(p))
	for i, token := range p {
		quoted[i] = quote(token)
	}
	return strings.Join(quoted, " ")
}

// key identifies a path in a map. NUL cannot occur in a configuration token.
func (p Path) key() string {
	return strings.Join(p, "\x00")
}

// HasPrefix reports whether prefix is p or an ancestor of it.
func (p Path) HasPrefix(prefix Path) bool {
	return len(p) >= len(prefix) && slices.Equal(p[:len(prefix)], prefix)
}

// quote wraps a token in single quotes unless it is safe to write bare.
func quote(token string) string {
	if token != "" && !strings.ContainsFunc(token, needsQuoting) {
		return token
	}
	return "'" + strings.ReplaceAll(token, "'", `'\''`) + "'"
}

func needsQuoting(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("._:/-+@,=%", r)
}

// ParseLine reads one "set" command. Quoting follows the shell, which is what
// VyOS prints and accepts: single quotes are literal, double quotes allow
// backslash escapes, adjacent pieces join.
func ParseLine(line string) (Path, error) {
	tokens, err := tokenize(line)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, errors.New("empty command")
	}
	if tokens[0] != "set" {
		// The configuration is declarative. "delete" has no place in it:
		// what should not be configured is left out.
		return nil, fmt.Errorf("commands have to start with \"set\", not %q", tokens[0])
	}
	if len(tokens) == 1 {
		return nil, errors.New("\"set\" without a path")
	}
	return Path(tokens[1:]), nil
}

func tokenize(line string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		inToken bool
	)
	flush := func() {
		if inToken {
			tokens = append(tokens, current.String())
			current.Reset()
			inToken = false
		}
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case ' ', '\t':
			flush()
		case '\'':
			end := slices.Index(runes[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			current.WriteString(string(runes[i+1 : i+1+end]))
			inToken = true
			i += end + 1
		case '"':
			inToken = true
			closed := false
			for i++; i < len(runes); i++ {
				if runes[i] == '"' {
					closed = true
					break
				}
				if runes[i] == '\\' && i+1 < len(runes) {
					i++
				}
				current.WriteRune(runes[i])
			}
			if !closed {
				return nil, errors.New("unterminated double quote")
			}
		case '\\':
			if i+1 >= len(runes) {
				return nil, errors.New("trailing backslash")
			}
			i++
			current.WriteRune(runes[i])
			inToken = true
		default:
			current.WriteRune(r)
			inToken = true
		}
	}
	flush()
	return tokens, nil
}

// Parse reads a configuration: one "set" command per line, empty lines and
// lines starting with # ignored. A command listed twice is returned once.
func Parse(text string) ([]Path, error) {
	var (
		paths []Path
		seen  = map[string]struct{}{}
	)
	for number, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		path, err := ParseLine(trimmed)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", number+1, err)
		}
		if _, dup := seen[path.key()]; dup {
			continue
		}
		seen[path.key()] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

// Lines renders paths as "set" commands.
func Lines(paths []Path) []string {
	lines := make([]string, len(paths))
	for i, path := range paths {
		lines[i] = path.String()
	}
	return lines
}

// Hash identifies a configuration regardless of the order of its commands.
func Hash(paths []Path) string {
	lines := Lines(paths)
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
