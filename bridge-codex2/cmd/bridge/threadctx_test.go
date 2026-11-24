package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// fakeThreadFetcher は get_thread の応答テキストを ParseThread に通して返す (SDK の GetThread と同じ経路)。
type fakeThreadFetcher struct {
	text  string // get_thread のテキスト結果
	err   error  // RPC 自体の失敗
	calls int
	limit int
}

func (f *fakeThreadFetcher) GetThread(_ context.Context, _ string, limit int) (*agenthub.Thread, error) {
	f.calls++
	f.limit = limit
	if f.err != nil {
		return nil, f.err
	}
	return agenthub.ParseThread(f.text)
}

func threadJSON(t *testing.T, msgs ...agenthub.Message) string {
	t.Helper()
	type wireMsg struct {
		ID        string  `json:"id"`
		From      string  `json:"from"`
		To        string  `json:"to"`
		Message   string  `json:"message"`
		CausedBy  *string `json:"caused_by"`
		Timestamp string  `json:"timestamp"`
	}
	wire := make([]wireMsg, 0, len(msgs))
	for _, m := range msgs {
		w := wireMsg{ID: m.ID, From: m.Sender, To: m.To, Message: m.Body, Timestamp: m.Timestamp}
		if m.CausedBy != "" {
			cb := m.CausedBy
			w.CausedBy = &cb
		}
		wire = append(wire, w)
	}
	b, err := json.Marshal(map[string]any{"root_id": msgs[0].ID, "thread_size": len(msgs), "messages": wire})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func chainMsg(id, causedBy, body string) agenthub.Message {
	return agenthub.Message{ID: id, Sender: "@ope", To: "@me", Body: body, CausedBy: causedBy,
		Timestamp: "2026-11-24T00:00:00.000Z"}
}

var defaultTestThreadCfg = threadContextConfig{MaxMessages: 10, MaxChars: 8000}

// TestThreadContextChain: A → B → C の C を処理すると A・B が古い順に添わり、C 自身と後の便は入らない。
func TestThreadContextChain(t *testing.T) {
	a := chainMsg("a", "", "BODY-A")
	b := chainMsg("b", "a", "BODY-B")
	c := chainMsg("c", "b", "BODY-C")
	d := chainMsg("d", "c", "BODY-D-LATER")
	f := &fakeThreadFetcher{text: threadJSON(t, a, b, c, d)}

	res := buildThreadContext(context.Background(), f, c, defaultTestThreadCfg)
	if res.Outcome != threadOutcomeAttached || res.Attached != 2 || res.Earlier != 2 {
		t.Fatalf("got outcome=%s attached=%d earlier=%d", res.Outcome, res.Attached, res.Earlier)
	}
	if f.limit != threadFetchLimit {
		t.Errorf("get_thread limit = %d, want %d", f.limit, threadFetchLimit)
	}
	ia, ib := strings.Index(res.Text, "BODY-A"), strings.Index(res.Text, "BODY-B")
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("A and B must appear in order: %q", res.Text)
	}
	if strings.Contains(res.Text, "BODY-C") || strings.Contains(res.Text, "BODY-D-LATER") {
		t.Errorf("current / later messages must not be attached: %q", res.Text)
	}
}

// TestThreadContextNoCausedBy: caused_by が空の便は hub を呼ばず、何も添えない (段 1 と同じ)。
func TestThreadContextNoCausedBy(t *testing.T) {
	f := &fakeThreadFetcher{}
	res := buildThreadContext(context.Background(), f, chainMsg("a", "", "x"), defaultTestThreadCfg)
	if res.Outcome != threadOutcomeNoChain || res.Text != "" || f.calls != 0 {
		t.Fatalf("got outcome=%s text=%q calls=%d", res.Outcome, res.Text, f.calls)
	}
}

// TestThreadContextServerErrorText: server が成功扱いの {"error":...} を返したら添えずに skipped
// (#340 着手 GO の条件 1)。
func TestThreadContextServerErrorText(t *testing.T) {
	f := &fakeThreadFetcher{text: `{"error":"スレッド r を閲覧する権限がありません"}`}
	res := buildThreadContext(context.Background(), f, chainMsg("c", "b", "x"), defaultTestThreadCfg)
	if res.Outcome != threadOutcomeSkipped || res.Text != "" {
		t.Fatalf("got outcome=%s text=%q", res.Outcome, res.Text)
	}
	if !strings.Contains(res.Reason, "権限がありません") {
		t.Errorf("reason should carry the server error: %q", res.Reason)
	}
}

// TestThreadContextRPCError: hub に届かない (RPC 失敗) ときも添えずに skipped。
func TestThreadContextRPCError(t *testing.T) {
	f := &fakeThreadFetcher{err: errors.New("dial tcp: connection refused")}
	res := buildThreadContext(context.Background(), f, chainMsg("c", "b", "x"), defaultTestThreadCfg)
	if res.Outcome != threadOutcomeSkipped || res.Text != "" || !strings.Contains(res.Reason, "connection refused") {
		t.Fatalf("got outcome=%s text=%q reason=%q", res.Outcome, res.Text, res.Reason)
	}
}

// TestThreadContextMessageNotInResult: 鎖が limit を超えてこの便が返らなかったら、古い側を添えずに skipped。
func TestThreadContextMessageNotInResult(t *testing.T) {
	f := &fakeThreadFetcher{text: threadJSON(t, chainMsg("a", "", "A"), chainMsg("b", "a", "B"))}
	res := buildThreadContext(context.Background(), f, chainMsg("z", "y", "x"), defaultTestThreadCfg)
	if res.Outcome != threadOutcomeSkipped || res.Text != "" {
		t.Fatalf("got outcome=%s text=%q", res.Outcome, res.Text)
	}
}

// TestThreadContextNoEarlier: この便より前の便が無い (自分が root と同等) なら no_chain。
func TestThreadContextNoEarlier(t *testing.T) {
	a := chainMsg("a", "x-gone", "A")
	f := &fakeThreadFetcher{text: threadJSON(t, a)}
	res := buildThreadContext(context.Background(), f, a, defaultTestThreadCfg)
	if res.Outcome != threadOutcomeNoChain || res.Text != "" {
		t.Fatalf("got outcome=%s text=%q", res.Outcome, res.Text)
	}
}

// TestThreadContextMaxMessages: 便数の上限を超える鎖では直近だけが添わる。
func TestThreadContextMaxMessages(t *testing.T) {
	msgs := []agenthub.Message{chainMsg("m0", "", "BODY-0")}
	for i := 1; i <= 5; i++ {
		msgs = append(msgs, chainMsg("m"+strconv.Itoa(i), "m"+strconv.Itoa(i-1), "BODY-"+strconv.Itoa(i)))
	}
	cur := msgs[5]
	f := &fakeThreadFetcher{text: threadJSON(t, msgs...)}
	res := buildThreadContext(context.Background(), f, cur, threadContextConfig{MaxMessages: 2, MaxChars: 8000})
	if res.Outcome != threadOutcomeAttached || res.Attached != 2 || res.Earlier != 5 {
		t.Fatalf("got outcome=%s attached=%d earlier=%d", res.Outcome, res.Attached, res.Earlier)
	}
	for _, want := range []string{"BODY-3", "BODY-4", "これより前の 3 便は上限のため省略"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("missing %q in %q", want, res.Text)
		}
	}
	for _, unwanted := range []string{"BODY-0", "BODY-1", "BODY-2", "BODY-5"} {
		if strings.Contains(res.Text, unwanted) {
			t.Errorf("unexpected %q in %q", unwanted, res.Text)
		}
	}
}

// TestThreadContextMaxChars: 文字数の上限で古い側が落ち、一番新しい便は上限を超えても末尾を切って載せる。
func TestThreadContextMaxChars(t *testing.T) {
	a := chainMsg("a", "", "AAAAAAAAAA")  // 10
	b := chainMsg("b", "a", "BBBBBBBBBB") // 10
	c := chainMsg("c", "b", "cur")
	f := &fakeThreadFetcher{text: threadJSON(t, a, b, c)}

	res := buildThreadContext(context.Background(), f, c, threadContextConfig{MaxMessages: 10, MaxChars: 15})
	if res.Attached != 1 || strings.Contains(res.Text, "AAAA") || !strings.Contains(res.Text, "BBBBBBBBBB") {
		t.Fatalf("15 chars: want only B, got attached=%d %q", res.Attached, res.Text)
	}

	res = buildThreadContext(context.Background(), f, c, threadContextConfig{MaxMessages: 10, MaxChars: 4})
	if res.Attached != 1 || !res.Truncated || !strings.Contains(res.Text, "(前略) BBBB\n") ||
		strings.Contains(res.Text, "BBBBB") {
		t.Fatalf("4 chars: want B tail only, got attached=%d truncated=%v %q", res.Attached, res.Truncated, res.Text)
	}
}

// TestFetchThreadContextLog: 添えた / 添えなかったが固定キー (msg "thread context"、outcome) で 1 行出る。
func TestFetchThreadContextLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	f := &fakeThreadFetcher{text: `{"error":"boom"}`}
	fetchThreadContext(context.Background(), f, chainMsg("c", "b", "x"), defaultTestThreadCfg)
	line := buf.String()
	for _, want := range []string{`level=WARN`, `msg="thread context"`, `msg_id=c`, `outcome=skipped`, `attached=0`, `reason=`} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %s", want, line)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Errorf("want exactly 1 log line, got: %s", line)
	}

	buf.Reset()
	f = &fakeThreadFetcher{text: threadJSON(t, chainMsg("a", "", "A"), chainMsg("b", "a", "B"))}
	fetchThreadContext(context.Background(), f, chainMsg("b", "a", "B"), defaultTestThreadCfg)
	line = buf.String()
	for _, want := range []string{`level=INFO`, `msg="thread context"`, `outcome=attached`, `attached=1`} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %s", want, line)
		}
	}
}

// TestFormatPromptThreadContext: 添える文字列は本文の指示のあと・footer の前に入り、空なら何も足さない。
func TestFormatPromptThreadContext(t *testing.T) {
	msg := chainMsg("c", "b", "BODY-C")
	footer := "@me [bridge] (operator-supervised)"
	got := formatPrompt("@me", msg, footer, "【同じ鎖の前の便 TEST】", time.Unix(0, 0))
	ib, it, iff := strings.Index(got, "BODY-C"), strings.Index(got, "【同じ鎖の前の便 TEST】"), strings.Index(got, footer)
	if ib < 0 || it < 0 || iff < 0 || !(ib < it && it < iff) {
		t.Fatalf("order body < thread ctx < footer not kept: %q", got)
	}
	if formatPrompt("@me", msg, footer, "", time.Unix(0, 0)) != strings.Replace(got, "\n\n【同じ鎖の前の便 TEST】", "", 1) {
		t.Error("empty thread ctx must leave the stage-1 prompt unchanged")
	}
}

// TestResolveThreadContextConfig: 上限の env 解決。不正値は起動エラー。
func TestResolveThreadContextConfig(t *testing.T) {
	t.Setenv(threadMaxMessagesEnv, "")
	t.Setenv(threadMaxCharsEnv, "")
	got, err := resolveThreadContextConfig()
	if err != nil || got != (threadContextConfig{MaxMessages: defaultThreadMaxMessages, MaxChars: defaultThreadMaxChars}) {
		t.Fatalf("defaults: got %+v err %v", got, err)
	}
	t.Setenv(threadMaxMessagesEnv, "3")
	t.Setenv(threadMaxCharsEnv, "100")
	got, err = resolveThreadContextConfig()
	if err != nil || got != (threadContextConfig{MaxMessages: 3, MaxChars: 100}) {
		t.Fatalf("set: got %+v err %v", got, err)
	}
	for _, bad := range []string{"0", "-1", "x"} {
		t.Setenv(threadMaxMessagesEnv, bad)
		if _, err := resolveThreadContextConfig(); err == nil {
			t.Errorf("%s=%q: want error", threadMaxMessagesEnv, bad)
		}
	}
}
