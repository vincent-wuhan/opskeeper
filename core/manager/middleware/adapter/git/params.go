// params.go holds the argument accessors and the validators that keep a
// model-supplied string from becoming a git option.
package git

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type params map[string]any

// revSyntax is the character set a revision may use.
//
// The set is deliberately a little wider than "a branch name" because
// `HEAD~1`, `HEAD^` and `v1.2.3` are ordinary things to ask for, and a
// grammar that refused them would push callers into passing a shell string
// instead. What it excludes is the revision *grammar*: `..` ranges, `^{}`
// peel, `@{n}` reflog and `:/text` search, each of which lets a caller name
// something other than what it appears to name.
var revSyntax = regexp.MustCompile(`^[A-Za-z0-9_./~^@][A-Za-z0-9_.\-/~^@]*$`)

// maxRevLength bounds a revision. A 40-hex SHA, a branch name and a tag all
// fit well inside this; anything longer is not a revision.
const maxRevLength = 200

// requireRev returns a validated revision.
func (p params) requireRev(name string) (string, error) {
	s, err := p.requireString(name)
	if err != nil {
		return "", err
	}
	return validateRev(name, s)
}

// optionalRev returns a validated revision, or "" when absent.
func (p params) optionalRev(name string) (string, error) {
	s, err := p.optionalString(name)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", nil
	}
	return validateRev(name, s)
}

func validateRev(name, value string) (string, error) {
	if len(value) > maxRevLength {
		return "", fmt.Errorf("git: %s is %d characters, longer than any revision (%d)", name, len(value), maxRevLength)
	}
	// A leading '-' is the shape of an option, and `git log --all` and
	// `git log -all` are different commands. Refusing rather than
	// escaping is the same call the k8s adapter makes for object names:
	// there is no legitimate revision that needs this.
	if strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("git: %s %q starts with '-' and would be parsed as an option", name, value)
	}
	// `..` is range syntax, `^{}` is peel syntax, `@{n}` is the reflog and
	// `:/text` searches commit messages. Each turns "this revision" into
	// "something related to this revision"; diff is the one tool that
	// takes a range, and it builds the range from two separately
	// validated revisions rather than accepting one string.
	//
	// These are checked before the character class so the message names
	// the actual problem: `HEAD^{}` is rejected for being peel syntax,
	// not for containing a brace.
	if strings.Contains(value, "..") || strings.Contains(value, "^{") ||
		strings.Contains(value, "@{") || strings.Contains(value, ":") {
		return "", fmt.Errorf("git: %s %q uses revision-range syntax, which this adapter does not accept", name, value)
	}
	if !revSyntax.MatchString(value) {
		return "", fmt.Errorf("git: %s %q is not a revision this adapter will resolve", name, value)
	}
	return value, nil
}

// requirePath returns a repository-relative path.
//
// Paths reach git after a `--` separator, so they cannot become options;
// what they can still do is escape the repository, and `git show
// HEAD:../../../etc/shadow` is a real way to read a file outside it.
func (p params) requirePath(name string) (string, error) {
	s, err := p.requireString(name)
	if err != nil {
		return "", err
	}
	return validatePath(name, s)
}

// optionalPath returns a validated path, or "" when absent.
func (p params) optionalPath(name string) (string, error) {
	s, err := p.optionalString(name)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", nil
	}
	return validatePath(name, s)
}

func validatePath(name, value string) (string, error) {
	if strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("git: %s %q is absolute; paths are repository-relative", name, value)
	}
	if strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("git: %s %q starts with '-'", name, value)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", fmt.Errorf("git: %s %q walks out of the repository", name, value)
		}
	}
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("git: %s contains a NUL byte", name)
	}
	return value, nil
}

// maxPatternLength bounds a search pattern. git grep takes a basic regular
// expression; the bounded length is what keeps a pathological one from
// turning a read into a hang, and the caller's timeout is the backstop.
const maxPatternLength = 512

func (p params) requirePattern(name string) (string, error) {
	s, err := p.requireString(name)
	if err != nil {
		return "", err
	}
	if len(s) > maxPatternLength {
		return "", fmt.Errorf("git: %s is longer than %d characters", name, maxPatternLength)
	}
	// The pattern is passed as its own argv entry and never through a
	// shell, so it cannot become a command. A leading '-' is still an
	// option to git grep, which is why it is refused here rather than
	// trusted to the `--` that follows.
	if strings.HasPrefix(s, "-") {
		return "", fmt.Errorf("git: %s %q starts with '-' and would be parsed as an option", name, s)
	}
	return s, nil
}

func (p params) requireString(name string) (string, error) {
	raw, ok := p[name]
	if !ok {
		return "", fmt.Errorf("git: %s is required", name)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("git: %s must be a string, got %T", name, raw)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("git: %s is required and must not be blank", name)
	}
	return s, nil
}

func (p params) optionalString(name string) (string, error) {
	raw, ok := p[name]
	if !ok || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("git: %s must be a string, got %T", name, raw)
	}
	return strings.TrimSpace(s), nil
}

// intArg reads an optional bounded integer. A limit of 0 or less is
// refused rather than clamped: "give me no history" is a caller mistake,
// and answering it with a default hides the mistake.
func intArg(p map[string]any, name string, def, max int) (int, error) {
	raw, ok := p[name]
	if !ok || raw == nil {
		return def, nil
	}
	var n int
	switch v := raw.(type) {
	case int:
		n = v
	case int32:
		n = int(v)
	case int64:
		n = int(v)
	case float64:
		n = int(v)
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("git: %s must be an integer, got %q", name, v.String())
		}
		n = int(i)
	default:
		return 0, fmt.Errorf("git: %s must be an integer, got %T", name, raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("git: %s must be positive, got %d", name, n)
	}
	if n > max {
		return 0, fmt.Errorf("git: %s is %d; this adapter returns at most %d", name, n, max)
	}
	return n, nil
}
