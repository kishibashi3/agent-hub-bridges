package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestStreamScanner_SkipsOversizeLine(t *testing.T) {
	const maxLine = 256
	big := `{"type":"user","message":{"content":"` + strings.Repeat("x", 1000) + `"}}`
	input := `{"type":"system"}` + "\n" + big + "\n" + `{"type":"assistant"}` + "\n"

	scanner := newStreamScanner(strings.NewReader(input), maxLine)
	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner.Err() = %v, want nil", err)
	}
	want := []string{`{"type":"system"}`, `{"type":"assistant"}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
}

func TestStreamScanner_LineExactlyAtLimitMinusNewline(t *testing.T) {
	const maxLine = 64
	line := strings.Repeat("a", maxLine-1)
	scanner := newStreamScanner(strings.NewReader(line+"\n"+"b\n"), maxLine)
	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner.Err() = %v", err)
	}
	if len(got) != 2 || got[0] != line || got[1] != "b" {
		t.Fatalf("tokens = %q", got)
	}
}

func TestStreamScanner_OversizeAtEOFWithoutNewline(t *testing.T) {
	const maxLine = 64
	input := `{"type":"system"}` + "\n" + `{"type":"user","x":"` + strings.Repeat("y", 500)
	scanner := newStreamScanner(strings.NewReader(input), maxLine)
	var got []string
	for scanner.Scan() {
		got = append(got, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner.Err() = %v", err)
	}
	if len(got) != 1 || got[0] != `{"type":"system"}` {
		t.Fatalf("tokens = %q", got)
	}
}

func TestReadUntilResult_OversizeUserEventContinues(t *testing.T) {
	const maxLine = 512
	big := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` +
		strings.Repeat("A", 10*maxLine) + `"}]}}`
	result := `{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"input_tokens":3,"output_tokens":4}}`
	input := `{"type":"assistant","message":{"model":"m","content":[]}}` + "\n" + big + "\n" + result + "\n"

	usage, err := readUntilResult(context.Background(), newStreamScanner(strings.NewReader(input), maxLine), io.Discard, nil, false)
	if err != nil {
		t.Fatalf("readUntilResult err = %v, want nil", err)
	}
	if usage.OutputTokens != 4 || usage.Model != "m" {
		t.Fatalf("usage = %+v", usage)
	}
}

// result 行自体が上限を超えても、result 待ちで止まらず turn を終えること。
func TestReadUntilResult_OversizeResultEvent(t *testing.T) {
	const maxLine = 512
	tests := []struct {
		name    string
		isError string
		wantErr bool
	}{
		{"success", "false", false},
		{"error", "true", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 実 CLI の result 行はキー順が一定でなく、ネストした "type" が先に現れ、
			// トップレベルの "type" は本文の後に来る (issue #299 で観測)。
			result := `{"duration_api_ms":1,"usage":{"iterations":[{"type":"message"}]},"is_error":` + tt.isError +
				`,"subtype":"` + tt.name + `","result":"` + strings.Repeat(`R\"`, 10*maxLine) +
				`","type":"result","session_id":"s"}`
			// result の後も stdout が閉じない (stdin 開放中) 状況を io.Pipe で再現する
			pr, pw := io.Pipe()
			defer pw.Close()
			go func() { _, _ = io.WriteString(pw, result+"\n") }()

			_, err := readUntilResult(context.Background(), newStreamScanner(pr, maxLine), io.Discard, nil, false)
			if (err != nil) != tt.wantErr {
				t.Fatalf("readUntilResult err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "exceeded scanner buffer") {
				t.Fatalf("err = %v, want message mentioning scanner buffer", err)
			}
		})
	}
}

func TestNewStreamScanner_ZeroMaxUsesDefault(t *testing.T) {
	scanner := newStreamScanner(strings.NewReader("hello\n"), 0)
	if !scanner.Scan() || scanner.Text() != "hello" {
		t.Fatalf("Scan/Text = %q, err=%v", scanner.Text(), scanner.Err())
	}
}

func TestTopLevelSniffer(t *testing.T) {
	line := `{"a":{"type":"nested","is_error":true},"is_error" : false,"list":["type"],` +
		`"subtype":"` + strings.Repeat("s", 100) + `","type":"re\"sult"}`
	s := newTopLevelSniffer("type", "subtype", "is_error")
	// 1 byte ずつ流し込んでも分割位置に依存しないこと
	for i := 0; i < len(line); i++ {
		s.write([]byte{line[i]})
	}
	if got := s.fields["type"]; got != `re\"sult` {
		t.Errorf("type = %q", got)
	}
	if got := s.fields["is_error"]; got != "false" {
		t.Errorf("is_error = %q", got)
	}
	if _, ok := s.fields["subtype"]; ok {
		t.Errorf("subtype (too long) should be dropped, got %q", s.fields["subtype"])
	}
}
