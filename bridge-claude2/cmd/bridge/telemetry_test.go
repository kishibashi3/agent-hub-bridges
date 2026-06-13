package main

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
)

// TestResolveSpanModel は span に乗せるモデル名の解決優先順位を検証する (issue #242)。
// 実応答モデル (response) > 要求モデル (request, = cfg.Model) > "claude-default" fallback。
func TestResolveSpanModel(t *testing.T) {
	tests := []struct {
		name          string
		responseModel string
		requestModel  string
		want          string
	}{
		{
			// AGENT_HUB_MODEL 未設定 (requestModel 空) でも実応答モデルが取れれば実モデル名。
			// これが issue #242 の本丸: claude-default 丸めの解消。
			name:          "response only (AGENT_HUB_MODEL unset) → actual model",
			responseModel: "claude-sonnet-4-6",
			requestModel:  "",
			want:          "claude-sonnet-4-6",
		},
		{
			// 両方ある場合は実応答モデルを優先 (要求と応答が食い違ってもログ上は実態が正)。
			name:          "response wins over request",
			responseModel: "claude-opus-4-8",
			requestModel:  "claude-sonnet-4-6",
			want:          "claude-opus-4-8",
		},
		{
			// 実応答が取れず要求モデルのみ → 要求モデルを尊重。
			name:          "request only → request model",
			responseModel: "",
			requestModel:  "claude-sonnet-4-6",
			want:          "claude-sonnet-4-6",
		},
		{
			// 完全に不明な場合のみ claude-default fallback (回帰なし)。
			name:          "neither → claude-default fallback",
			responseModel: "",
			requestModel:  "",
			want:          "claude-default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveSpanModel(tt.responseModel, tt.requestModel); got != tt.want {
				t.Errorf("resolveSpanModel(%q, %q) = %q, want %q",
					tt.responseModel, tt.requestModel, got, tt.want)
			}
		})
	}
}

// TestReadUntilResult_ExtractsModel は stream-json の assistant メッセージから
// 実応答モデル名を queryUsage.Model に抽出することを検証する (issue #242)。
func TestReadUntilResult_ExtractsModel(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		wantModel string
		wantIn    int
		wantOut   int
	}{
		{
			name: "assistant.message.model is captured",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}]}}`,
				`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2},"total_cost_usd":0.01}`,
			},
			wantModel: "claude-sonnet-4-6",
			wantIn:    10,
			wantOut:   5,
		},
		{
			name: "multiple assistant events → last model wins",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"a"}]}}`,
				`{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`,
				`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":3,"output_tokens":1}}`,
			},
			wantModel: "claude-sonnet-4-6",
			wantIn:    3,
			wantOut:   1,
		},
		{
			name: "no model field → empty (emitSpan will fallback)",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
				`{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}`,
			},
			wantModel: "",
			wantIn:    1,
			wantOut:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scanner := bufio.NewScanner(strings.NewReader(strings.Join(tt.lines, "\n") + "\n"))
			usage, err := readUntilResult(context.Background(), scanner, io.Discard, nil, false)
			if err != nil {
				t.Fatalf("readUntilResult returned error: %v", err)
			}
			if usage.Model != tt.wantModel {
				t.Errorf("usage.Model = %q, want %q", usage.Model, tt.wantModel)
			}
			if usage.InputTokens != tt.wantIn {
				t.Errorf("usage.InputTokens = %d, want %d", usage.InputTokens, tt.wantIn)
			}
			if usage.OutputTokens != tt.wantOut {
				t.Errorf("usage.OutputTokens = %d, want %d", usage.OutputTokens, tt.wantOut)
			}
		})
	}
}
