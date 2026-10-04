package github

import (
	"reflect"
	"testing"
)

// parseTarget must resolve each claimed URL to the right kind and fields.
func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want target
	}{
		{"https://github.com/o/r/issues/7", target{kind: kindIssue, owner: "o", repo: "r", number: "7"}},
		{"https://github.com/o/r/pull/9", target{kind: kindPull, owner: "o", repo: "r", number: "9", pull: true}},
		{"https://github.com/longhorn/longhorn", target{kind: kindRepo, owner: "longhorn", repo: "longhorn"}},
		{"https://GitHub.com/o/r.js/", target{kind: kindRepo, owner: "o", repo: "r.js"}},
		{"https://github.com/o/r/blob/main/docs/a.md", target{kind: kindBlob, owner: "o", repo: "r", rest: "main/docs/a.md"}},
		{"https://github.com/o/r/blob/main/a.go#L10-L20", target{kind: kindBlob, owner: "o", repo: "r", rest: "main/a.go"}},
		{"https://github.com/o/r/tree/feature/x/src", target{kind: kindTree, owner: "o", repo: "r", rest: "feature/x/src"}},
		{"https://github.com/o/r/tree/main", target{kind: kindTree, owner: "o", repo: "r", rest: "main"}},
		{"https://github.com/o/r/releases", target{kind: kindReleases, owner: "o", repo: "r"}},
		{"https://github.com/o/r/releases/latest", target{kind: kindLatestRelease, owner: "o", repo: "r"}},
		{"https://github.com/o/r/releases/tag/v1.2.3", target{kind: kindRelease, owner: "o", repo: "r", tag: "v1.2.3"}},
		{"https://github.com/o/r/releases/tag/pkg/v1", target{kind: kindRelease, owner: "o", repo: "r", tag: "pkg/v1"}},
		{"https://github.com/o/r/discussions/42", target{kind: kindDiscussion, owner: "o", repo: "r", number: "42"}},
		{"https://github.com/o/r/commit/06c08d2", target{kind: kindCommit, owner: "o", repo: "r", sha: "06c08d2"}},
		{"https://github.com/o/r/pull/3/commits/06c08d2f", target{kind: kindCommit, owner: "o", repo: "r", sha: "06c08d2f"}},
		{"https://gist.github.com/schacon/1", target{kind: kindGist, gistID: "1"}},
		{"https://gist.github.com/aa5a315d61ae9438b18d0123456789ab", target{kind: kindGist, gistID: "aa5a315d61ae9438b18d0123456789ab"}},
	} {
		got, ok := parseTarget(tc.url)
		if !ok {
			t.Errorf("parseTarget(%q) not claimed", tc.url)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseTarget(%q) = %+v, want %+v", tc.url, got, tc.want)
		}
	}
}

// A ref may contain "/", so a blob/tree URL yields several ref/path guesses,
// shortest ref first, bounded by maxRefSegments; blobs need a non-empty path.
func TestRefSplits(t *testing.T) {
	for _, tc := range []struct {
		rest     string
		needPath bool
		want     []refPath
	}{
		{"main/a.go", true, []refPath{{"main", "a.go"}}},
		{"feature/x/src/a.go", true, []refPath{{"feature", "x/src/a.go"}, {"feature/x", "src/a.go"}, {"feature/x/src", "a.go"}}},
		{"main", false, []refPath{{"main", ""}}},
		{"feature/x", false, []refPath{{"feature", "x"}, {"feature/x", ""}}},
		{"a/b/c/d/e", false, []refPath{{"a", "b/c/d/e"}, {"a/b", "c/d/e"}, {"a/b/c", "d/e"}}},
	} {
		if got := refSplits(tc.rest, tc.needPath); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("refSplits(%q, %v) = %v, want %v", tc.rest, tc.needPath, got, tc.want)
		}
	}
}
