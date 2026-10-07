package redis

// scanAndDelete removes the keys that are too large to be a mistake of
// arithmetic.
//
// The safety here is structural rather than advisory. A single argument —
// min_bytes — decides what counts as too big, and it is **required**, so
// there is no default that can quietly turn into "delete everything". The
// caller has to state a size, which means the number that justifies
// deleting data was chosen by whoever approved the action rather than
// written into this file as a constant that is right until it is not.
//
// Two further limits keep one call bounded. A key cap means a runaway
// threshold cannot wipe a keyspace in a single action, and the cap is
// checked *before* deleting, so an over-large request is refused rather
// than partially honoured. And the scan is a sample, not a census:
// Redis has no key-size index, so what this walks is whatever SCAN
// returned, and the report says so — a tool that deleted "every big key"
// while having sampled a few percent of the keyspace would be describing
// something it did not do.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const (
	// maxDeletePerCall bounds how many keys one action may remove.
	maxDeletePerCall = 100

	// defaultDeleteBatch is well under the cap so the common case needs no
	// argument at all.
	defaultDeleteBatch = 20
)

func (a *Adapter) scanAndDelete(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	// Required on purpose — see the file comment. This is the decision.
	minBytes, err := p.requireInt("min_bytes")
	if err != nil {
		return 0, "", false, err
	}
	if minBytes <= 0 {
		return 0, "", false, fmt.Errorf("redis: min_bytes must be greater than zero; a floor of zero matches every " +
			"key in the database, and this tool exists to remove oversized ones rather than to empty a keyspace")
	}
	// A missing pattern is not an error: scanning everything is the normal
	// case for a memory alert. A *blank* one is, because a pattern that
	// matches nothing would report "nothing deleted" and look like a
	// successful remediation of a keyspace it never looked at.
	pattern := "*"
	if raw, ok := p["pattern"]; ok && raw != nil {
		text, isString := raw.(string)
		if !isString {
			return 0, "", false, fmt.Errorf("redis: pattern must be a string, got %T", raw)
		}
		pattern = strings.TrimSpace(text)
		if pattern == "" {
			return 0, "", false, fmt.Errorf(`redis: pattern must not be blank; use "*" for every key`)
		}
	}
	limit, err := intArg(p, "limit", defaultDeleteBatch)
	if err != nil {
		return 0, "", false, err
	}
	if limit > maxDeletePerCall {
		return 0, "", false, fmt.Errorf("redis: limit %d exceeds the %d-key ceiling for a single action; "+
			"delete in batches so each one is separately visible in the audit chain", limit, maxDeletePerCall)
	}
	scanLimit, err := intArg(p, "scan_limit", defaultScanLimit)
	if err != nil {
		return 0, "", false, err
	}
	dryRun := false
	if raw, ok := p["dry_run"]; ok && raw != nil {
		b, isBool := raw.(bool)
		if !isBool {
			return 0, "", false, fmt.Errorf("redis: dry_run must be a boolean, got %T", raw)
		}
		dryRun = b
	}

	keys, sampled, err := a.scanKeys(ctx, pattern, scanLimit)
	if err != nil {
		return 0, "", false, err
	}
	type sized struct {
		key   string
		bytes int64
		typ   string
	}
	candidates := make([]sized, 0, len(keys))
	for _, key := range keys {
		size, sizeErr := client.MemoryUsage(ctx, key).Result()
		if sizeErr != nil {
			// A key can expire between SCAN and MEMORY USAGE. Skipping it
			// is the same reasoning the read path uses, and for a delete
			// it is the safer direction: a key we could not measure is a
			// key we did not touch.
			continue
		}
		if size < int64(minBytes) {
			continue
		}
		typ, _ := client.Type(ctx, key).Result()
		candidates = append(candidates, sized{key: key, bytes: size, typ: typ})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].bytes > candidates[j].bytes })

	oversized := len(candidates)
	if oversized > limit {
		// Truncate rather than refuse: the caller asked for a batch, and
		// the report says how many matched in total so the truncation is
		// visible instead of looking like the whole job finished.
		candidates = candidates[:limit]
	}
	if len(candidates) == 0 {
		return 0, fmt.Sprintf("no key matching %q under %d bytes among %d sampled; nothing deleted",
			pattern, minBytes, sampled), true, nil
	}

	var reclaimed int64
	removed := make([]string, 0, len(candidates))
	if dryRun {
		for _, c := range candidates {
			reclaimed += c.bytes
			removed = append(removed, c.key)
		}
		return len(removed), fmt.Sprintf("dry run: %d key(s) matching %q at or above %d bytes would be deleted "+
			"(%s reclaimable), out of %d found among %d sampled: %s",
			len(removed), pattern, minBytes, humanBytes(reclaimed), oversized, sampled, previewKeys(removed)), true, nil
	}
	var failed []string
	for _, c := range candidates {
		deleted, delErr := client.Del(ctx, c.key).Result()
		if delErr != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", c.key, delErr))
			continue
		}
		if deleted == 0 {
			// It expired between the scan and the delete. The goal is met
			// for that key, but saying "deleted" would be a claim about an
			// event that did not happen.
			removed = append(removed, c.key+" (already gone)")
			continue
		}
		reclaimed += c.bytes
		removed = append(removed, c.key)
	}
	message := fmt.Sprintf("deleted %d key(s) matching %q at or above %d bytes, %s reclaimable",
		len(removed)-countGone(removed), pattern, minBytes, humanBytes(reclaimed))
	if oversized > len(candidates) {
		message += fmt.Sprintf("; %d more matched than this batch's limit of %d", oversized-len(candidates), limit)
	}
	message += fmt.Sprintf(", sampled %d key(s) — Redis has no key-size index, so this was a sample not a census: %s",
		sampled, previewKeys(removed))
	if len(failed) > 0 {
		return len(removed) - countGone(removed), message + fmt.Sprintf("; %d failed: %s", len(failed), strings.Join(failed, "; ")), false, nil
	}
	return len(removed) - countGone(removed), message, true, nil
}

func countGone(removed []string) int {
	n := 0
	for _, entry := range removed {
		if strings.HasSuffix(entry, "(already gone)") {
			n++
		}
	}
	return n
}

// previewKeys names a bounded sample: a keyspace with ten thousand
// oversized keys would otherwise put ten thousand names into one message
// that lands in an incident transcript.
func previewKeys(keys []string) string {
	const sample = 5
	if len(keys) <= sample {
		return strings.Join(keys, ", ")
	}
	return fmt.Sprintf("%s, … and %d more", strings.Join(keys[:sample], ", "), len(keys)-sample)
}

// humanBytes renders a byte count for a human reader.
//
// The exact figure is kept alongside it. "reclaimed 1.0 GB" and "reclaimed
// 1073741824 bytes" are the same fact, and an operator deciding whether to
// widen a threshold wants both — the rounded number is what a budget is
// written in, the exact one is what a post-incident review is reconciled
// against.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB (%d bytes)", float64(n)/float64(div), "KMGTP"[exp], n)
}
