// Package gatename holds the two rules that decide what counts as an
// acceptance gate and which make targets exist.
//
// They live here because two commands need them and the reason they must
// agree is not tidiness. cigate reports a check-shaped target nobody runs;
// the gate report runs the check-shaped targets. If each carried its own
// copy of the naming rule, a target could be in one command's set and not
// the other's, and the two would then disagree about the same repository
// without either being able to see it — which is the exact shape decision
// 462 spent a knife on at one level up, where a census and a gate read the
// same cell and reached different conclusions about it.
package gatename

import "strings"

// LooksLikeGate is the naming shape a target must have to be treated as an
// acceptance gate: it ends in -check, or it is exactly check.
//
// The shape is deliberately loose. A target that reads like a gate to
// someone scanning the Makefile is one they will assume somebody runs, so
// the question worth asking is not "is it one of the promised gates" but
// "is it gate-shaped and unaccounted for". Being strict here would make an
// exemption invisible.
func LooksLikeGate(target string) bool {
	return strings.HasSuffix(target, "-check") || target == "check"
}

// MakeTargets is every target the Makefile defines.
//
// The rules, and why each is here rather than left to a reader of the
// output:
//
//   - a recipe line is indented, and a recipe that happened to contain a
//     colon must not become a target;
//   - `:=` and `::=` and friends are variable assignments, not targets, so
//     the character right after the first colon decides;
//   - a leading dot is make's own syntax (`.PHONY`, `.DEFAULT_GOAL`) and a
//     target whose name carries `=`, `?` or `$` is one this parser cannot
//     name, so it is left out rather than half-read.
//
// A target this parser cannot name is excluded rather than guessed at,
// because the alternative is a report that claims to have run something
// nobody can invoke.
func MakeTargets(src string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(src, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		i := strings.IndexByte(line, ':')
		if i < 0 || i+1 < len(line) && line[i+1] == '=' {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "=?$") {
			continue
		}
		out[name] = true
	}
	return out
}
