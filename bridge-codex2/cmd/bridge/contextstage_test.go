package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestResolveContextStage は AGENT_HUB_CONTEXT_STAGE の解決を確認する (issue #339)。
// 値は bridge-claude2 と揃えている。
func TestResolveContextStage(t *testing.T) {
	cases := []struct {
		env     string
		want    contextStage
		wantErr bool
	}{
		{env: "", want: contextStageNone},
		{env: "none", want: contextStageNone},
		{env: "last", want: contextStageLast},
		{env: "chain", wantErr: true},
		{env: "NONE", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv(contextStageEnv, c.env)
			got, err := resolveContextStage()
			if c.wantErr {
				if err == nil {
					t.Fatalf("env %q: want error, got %q", c.env, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("env %q: unexpected error: %v", c.env, err)
			}
			if got != c.want {
				t.Fatalf("env %q: got %q, want %q", c.env, got, c.want)
			}
		})
	}
}

// runStageQueries は段 stage の runner で query を 2 回流し、codex に渡った引数を 1 行ずつ返す。
// marker file を先に置いておけるよう、withMarker を受け取る。
func runStageQueries(t *testing.T, stage contextStage, withMarker bool) (args []string, markerExists bool) {
	t.Helper()
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	script := filepath.Join(dir, "fake-codex.sh")
	body := "#!/bin/sh\necho \"$*\" >> " + argsLog + "\ncat > /dev/null\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, sessionMarkerFile)
	if withMarker {
		if err := os.WriteFile(marker, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config{
		CodexCLI:          script,
		Workdir:           dir,
		CodexHomeDir:      dir,
		SubprocessTimeout: 10 * time.Second,
		ScannerBufferSize: 64 * 1024,
		ContextStage:      stage,
	}
	r := newCodexRunner(cfg)
	for i := 0; i < 2; i++ {
		if _, err := r.query(context.Background(), "hi", "", nil); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	args = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	_, statErr := os.Stat(marker)
	return args, statErr == nil
}

func isResume(args string) bool {
	return strings.HasPrefix(args, "exec resume --last")
}

// TestContextStageNone_NewSessionEveryMessage は段 1 (none) で毎回新しいセッションになり、
// 既存の marker file があっても resume しないことを確認する (issue #339)。
func TestContextStageNone_NewSessionEveryMessage(t *testing.T) {
	for _, withMarker := range []bool{false, true} {
		args, markerExists := runStageQueries(t, contextStageNone, withMarker)
		if len(args) != 2 {
			t.Fatalf("withMarker=%v: want 2 codex runs, got %d: %q", withMarker, len(args), args)
		}
		for i, a := range args {
			if isResume(a) {
				t.Errorf("withMarker=%v: run %d resumed a session: %q", withMarker, i, a)
			}
		}
		if markerExists != withMarker {
			t.Errorf("withMarker=%v: marker exists after runs = %v; stage none must not write or remove it",
				withMarker, markerExists)
		}
	}
}

// TestContextStageLast_ResumesAfterFirst は旧挙動 (last) で 2 回目から resume --last になり、
// marker file を書くことを確認する。marker が既にあれば 1 回目から resume する。
func TestContextStageLast_ResumesAfterFirst(t *testing.T) {
	args, markerExists := runStageQueries(t, contextStageLast, false)
	if len(args) != 2 || isResume(args[0]) || !isResume(args[1]) {
		t.Fatalf("want [new, resume], got %q", args)
	}
	if !markerExists {
		t.Error("stage last must write the session marker")
	}

	args, _ = runStageQueries(t, contextStageLast, true)
	if len(args) != 2 || !isResume(args[0]) || !isResume(args[1]) {
		t.Fatalf("with marker: want [resume, resume], got %q", args)
	}
}
