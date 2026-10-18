package main

import "testing"

// TestResolveContextStage は AGENT_HUB_CONTEXT_STAGE の解決を確認する (issue #339)。
// 値は bridge-codex2 と揃えている。
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
