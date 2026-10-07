package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGrepFile_ParamsRequired(t *testing.T) {
	_, err := GrepFile{}.Execute(context.Background(), []byte(`{"path":"/etc/hostname"}`))
	if err == nil {
		t.Error("expected error for missing pattern")
	}
}

func TestGrepFile_PathMustBeAbsolute(t *testing.T) {
	_, err := GrepFile{}.Execute(context.Background(), []byte(`{"path":"relative","pattern":"foo"}`))
	if err == nil {
		t.Error("expected error for relative path")
	}
}

func TestGrepFile_PathNoTraversal(t *testing.T) {
	_, err := GrepFile{}.Execute(context.Background(), []byte(`{"path":"/etc/../shadow","pattern":"foo"}`))
	if err == nil {
		t.Error("expected error for traversal path")
	}
}

func TestGrepFile_BasicMatch(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "test.log")
	content := "line one\nfoo bar\nline three\nfoo baz\n"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := GrepFile{}.Execute(context.Background(), []byte(`{"path":"`+tmp+`","pattern":"foo"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	res := grepFileResult{}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2", res.TotalMatches)
	}
	if res.Matches[0].LineNum != 2 {
		t.Errorf("Matches[0].LineNum = %d, want 2", res.Matches[0].LineNum)
	}
}

func TestGrepFile_Truncated(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "test.log")
	content := ""
	for i := 0; i < 200; i++ {
		content += "foo line " + itoa(i) + "\n"
	}
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := GrepFile{}.Execute(context.Background(), []byte(`{"path":"`+tmp+`","pattern":"foo","max_matches":10}`))
	res := grepFileResult{}
	_ = json.Unmarshal(out, &res)
	if !res.Truncated {
		t.Error("expected Truncated=true")
	}
	if len(res.Matches) != 10 {
		t.Errorf("len(Matches) = %d, want 10", len(res.Matches))
	}
}

func TestGrepFile_IgnoreCase(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "test.log")
	if err := os.WriteFile(tmp, []byte("FOO\nbar\nFoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := GrepFile{}.Execute(context.Background(), []byte(`{"path":"`+tmp+`","pattern":"foo","ignore_case":true}`))
	res := grepFileResult{}
	_ = json.Unmarshal(out, &res)
	if res.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2 (FOO + Foo)", res.TotalMatches)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
