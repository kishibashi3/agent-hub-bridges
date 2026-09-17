package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func scanAll(t *testing.T, input string, maxLine int) []string {
	t.Helper()
	scanner := newStreamScanner(strings.NewReader(input), maxLine)
	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner.Err() = %v, want nil", err)
	}
	return got
}

func TestStreamScanner_SkipsOversizeLine(t *testing.T) {
	big := `{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"` + strings.Repeat("x", 1000) + `"}}`
	input := `{"type":"turn.started"}` + "\n" + big + "\n" + `{"type":"turn.completed"}` + "\n"
	got := scanAll(t, input, 256)
	want := []string{`{"type":"turn.started"}`, `{"type":"turn.completed"}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
}

func TestStreamScanner_LineExactlyAtLimitMinusNewline(t *testing.T) {
	const maxLine = 64
	line := strings.Repeat("a", maxLine-1)
	got := scanAll(t, line+"\n"+"b\n", maxLine)
	if len(got) != 2 || got[0] != line || got[1] != "b" {
		t.Fatalf("tokens = %q", got)
	}
}

func TestStreamScanner_OversizeAtEOFWithoutNewline(t *testing.T) {
	input := `{"type":"turn.started"}` + "\n" + `{"type":"item.completed","x":"` + strings.Repeat("y", 500)
	got := scanAll(t, input, 64)
	if len(got) != 1 || got[0] != `{"type":"turn.started"}` {
		t.Fatalf("tokens = %q", got)
	}
}

func TestNewStreamScanner_NonPositiveMaxUsesDefaultAndWarns(t *testing.T) {
	for _, maxLine := range []int{0, -1} {
		var logBuf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
		// 4MB 未満の上限だと読み捨てられる長さの行で、既定値 (4MB) が使われていることを確かめる
		line := strings.Repeat("x", 1024*1024)
		scanner := newStreamScanner(strings.NewReader(line+"\n"), maxLine)
		ok := scanner.Scan()
		slog.SetDefault(prev)
		if !ok || scanner.Text() != line {
			t.Fatalf("maxLine=%d: Scan=%v len=%d err=%v", maxLine, ok, len(scanner.Text()), scanner.Err())
		}
		out := logBuf.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "scanner buffer size is not positive") ||
			!strings.Contains(out, fmt.Sprintf("scanner_buffer_size=%d", maxLine)) ||
			!strings.Contains(out, fmt.Sprintf("default=%d", defaultScannerBufferSize)) {
			t.Fatalf("maxLine=%d: log = %q, want WARN with scanner_buffer_size and default", maxLine, out)
		}
	}
}

func TestTopLevelSniffer(t *testing.T) {
	line := `{"item":{"type":"nested"},"list":["type"],"type":"item.com\"pleted"}`
	s := newTopLevelSniffer("type")
	// 1 byte ずつ流し込んでも分割位置に依存しないこと
	for i := 0; i < len(line); i++ {
		s.write([]byte{line[i]})
	}
	if got := s.fields["type"]; got != `item.com\"pleted` {
		t.Errorf("type = %q", got)
	}
}

// TestQuery_OversizeLineDoesNotHang: 上限を超える行のあとに出力が続いても、stdout を読み続けて
// codex が自分で終了できること (issue #302: 以前は読み取りが止まり SubprocessTimeout で kill されていた)。
func TestQuery_OversizeLineDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	body := "#!/bin/bash\n" +
		"cat >/dev/null\n" +
		"echo '{\"type\":\"turn.started\"}'\n" +
		"printf '{\"type\":\"item.completed\",\"out\":\"'; head -c 200000 /dev/zero | tr '\\0' x; echo '\"}'\n" +
		"for i in $(seq 1 20); do head -c 100000 /dev/zero | tr '\\0' y; echo; done\n" +
		"echo '{\"type\":\"turn.completed\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config{
		CodexCLI:          script,
		Workdir:           dir,
		CodexHomeDir:      dir,
		SubprocessTimeout: 10 * time.Second,
		ScannerBufferSize: 64 * 1024,
	}
	r := newCodexRunner(cfg)
	start := time.Now()
	if _, err := r.query(context.Background(), "hi", "", nil); err != nil {
		t.Fatalf("query err = %v (elapsed %v)", err, time.Since(start))
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("query took %v; want to finish without waiting for SubprocessTimeout", elapsed)
	}
}
