#!/usr/bin/env python3
"""Make indigo's atproto/syntax safe for Safari Web Workers.

Go's regexp compiler recurses once per level of a bounded repeat such as
x{1,512}, and atproto/syntax compiles its validators at package init. Safari's
Web Worker stack is too small for that: the module dies with "RangeError:
Maximum call stack size exceeded" before main runs. Rewrite the repeats as
unbounded ones and enforce the limits they dropped in code, so validation is
unchanged: indigo already checks the lengths of record keys, CIDs and AT-URI
record keys (via ParseRecordKey); the label and URI-scheme limits are added
in wasm_lengths.go.

Usage: patch-indigo-syntax-js.py <indigo module copy>
Fails if a patch target moved; a no-op on an already patched copy.
"""
import os
import sys

syntax = os.path.join(sys.argv[1], "atproto", "syntax")
edits = {
    "recordkey.go": [("{1,512}$`", "+$`")],
    "cid.go": [("{8,256}$`", "+$`")],
    "aturi.go": [("[a-zA-Z0-9_~.:-]{1,512}))", "[a-zA-Z0-9_~.:-]+))")],
    "uri.go": [("[a-z.-]{0,80}:", "[a-z.-]*:"),
               ("\tif !uriRegex.MatchString(raw) {", "\tif !uriRegex.MatchString(raw) || !schemeWithin(raw, 81) {")],
    "handle.go": [("{0,61}", "*"),
                  ("\tif !handleRegex.MatchString(raw) {", "\tif !handleRegex.MatchString(raw) || !labelsWithin(raw, 63) {")],
    "nsid.go": [("{0,61}", "*"), ("([a-zA-Z0-9]{0,62})?", "[a-zA-Z0-9]*"),
                ("\tif !nsidRegex.MatchString(raw) {", "\tif !nsidRegex.MatchString(raw) || !labelsWithin(raw, 63) {")],
}
for name, subs in edits.items():
    path = os.path.join(syntax, name)
    text = open(path).read()
    for old, new in subs:
        if old in text:
            text = text.replace(old, new)
        elif new not in text:
            sys.exit(f"{path}: patch target {old!r} not found")
    open(path, "w").write(text)

open(os.path.join(syntax, "wasm_lengths.go"), "w").write('''package syntax

// labelsWithin reports whether every dot-separated label of s is at most max
// bytes, the limit the upstream {0,61}/{0,62} repeats enforced.
func labelsWithin(s string, max int) bool {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			n = 0
			continue
		}
		if n++; n > max {
			return false
		}
	}
	return true
}

// schemeWithin reports whether the URI scheme before the first ':' is at most
// max bytes, the limit the upstream [a-z][a-z.-]{0,80} enforced.
func schemeWithin(s string, max int) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i <= max
		}
	}
	return false
}
''')
