#!/usr/bin/env bash
# Check that scripts/patch-indigo-syntax-js.py keeps indigo's atproto/syntax
# validation unchanged: builds the original and the patched package side by
# side natively and compares their Parse* results on boundary cases.
# Usage: check-indigo-syntax-patch.sh [indigo module dir]
set -euo pipefail
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
indigo="${1:-$(cd "${repo_root}/socialweb-wasm" 2>/dev/null && go list -m -f '{{.Dir}}' github.com/bluesky-social/indigo)}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/orig" "${tmp}/patched/atproto"
cp "${indigo}"/atproto/syntax/*.go "${tmp}/orig/"
cp -R "${indigo}/atproto/syntax" "${tmp}/patched/atproto/"
chmod -R u+w "${tmp}"
rm -f "${tmp}"/orig/*_test.go "${tmp}"/patched/atproto/syntax/*_test.go "${tmp}/orig/wasm_lengths.go"
rm -rf "${tmp}/patched/atproto/syntax/cmd" "${tmp}/patched/atproto/syntax/testdata"
python3 "${repo_root}/scripts/patch-indigo-syntax-js.py" "${tmp}/patched"
grep -q '{1,512}' "${tmp}/orig/recordkey.go" || { echo "original copy is already patched: ${indigo}" >&2; exit 1; }
printf 'module check\n\ngo 1.22\n' > "${tmp}/go.mod"
cat > "${tmp}/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"
	"strings"

	orig "check/orig"
	patched "check/patched/atproto/syntax"
)

func main() {
	r := strings.Repeat
	label := func(n int) string { return "a" + r("b", n-1) }
	var cases []string
	for _, n := range []int{1, 2, 61, 62, 63, 64, 65} {
		l := label(n)
		cases = append(cases, l+".com", "x."+l+".com", "x.y."+l, "com.example."+l, l+".example.fooBar",
			"com."+l+".x", "-"+l+".com", l+"-.com", "a.b.c"+r("0", n), "com.example.f"+r("0", n-1))
	}
	for _, n := range []int{1, 7, 8, 9, 255, 256, 257, 511, 512, 513} {
		cases = append(cases, r("a", n), r("Z9+=", n/4+1)[:n], "at://did:plc:abc/com.example.x/"+r("k", n))
	}
	for _, n := range []int{1, 2, 79, 80, 81, 82, 83} {
		cases = append(cases, "a"+r("b", n-1)+":x", "a"+r(".", n-1)+":x", "a"+r("-", n-1)+":/p", r("b", n)+"::")
	}
	cases = append(cases, "", ".", "a", "a.b", "a..b", "1.com", "a.1", "jay.bsky.social", "jay.bsky.social.",
		"com.example.fooBar", "com.example.foo-bar", "com.example.*", "com.example.f1", "com.1example.x", "com.example",
		"https://example.com", "at://did:plc:abc", "at://handle.example/app.bsky.feed.post/3jzfcijpj2z2a",
		"at://handle.example/app.bsky.feed.post/", "at://h.e/c.o.l/!", "did:plc:abc", "Https://x", "a:", "a: b",
		"bafyreidfayvfuwqa7qlnopdjiqrxzs6blmoeu4rujcjtnci5beludirz2a", "self", ".", "..", "a/b", "rkey~:._-",
		"x."+r("a", 63)+"."+r("b", 63)+"."+r("c", 63)+".com", strings.Repeat("a.", 126)+"bc", strings.Repeat("a.", 127)+"bc")
	type pair struct {
		name string
		o, p func(string) error
	}
	fns := []pair{
		{"ParseHandle", func(s string) error { _, e := orig.ParseHandle(s); return e }, func(s string) error { _, e := patched.ParseHandle(s); return e }},
		{"ParseNSID", func(s string) error { _, e := orig.ParseNSID(s); return e }, func(s string) error { _, e := patched.ParseNSID(s); return e }},
		{"ParseURI", func(s string) error { _, e := orig.ParseURI(s); return e }, func(s string) error { _, e := patched.ParseURI(s); return e }},
		{"ParseRecordKey", func(s string) error { _, e := orig.ParseRecordKey(s); return e }, func(s string) error { _, e := patched.ParseRecordKey(s); return e }},
		{"ParseCID", func(s string) error { _, e := orig.ParseCID(s); return e }, func(s string) error { _, e := patched.ParseCID(s); return e }},
		{"ParseATURI", func(s string) error { _, e := orig.ParseATURI(s); return e }, func(s string) error { _, e := patched.ParseATURI(s); return e }},
	}
	bad, valid := 0, 0
	for _, f := range fns {
		for _, c := range cases {
			oe, pe := f.o(c), f.p(c)
			if (oe == nil) != (pe == nil) {
				bad++
				fmt.Printf("MISMATCH %s(%q): original err=%v patched err=%v\n", f.name, c, oe, pe)
			} else if oe == nil {
				valid++
			}
		}
	}
	fmt.Printf("%d functions x %d inputs, %d accepted by both, %d mismatches\n", len(fns), len(cases), valid, bad)
	if bad > 0 {
		os.Exit(1)
	}
}
EOF
cd "${tmp}" && GOFLAGS=-mod=mod go run .
