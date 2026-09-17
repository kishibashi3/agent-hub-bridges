// scanner.go — claude stream-json 用 bufio.Scanner (issue #299)
//
// bufio.Scanner は 1 行がバッファ上限を超えると ErrTooLong で読み取りを止める。
// 画像や大きなファイルの tool_result を含む行でこれが起きると turn 全体がエラーになるため、
// 上限を超えた行は読み捨てて処理を続け、何が超えたか (イベント種別・行サイズ) をログに残す。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// oversizeHeadBytes は読み捨てる行の先頭をログに出すために保持するバイト数。
const oversizeHeadBytes = 200

// defaultScannerBufferSize は AGENT_HUB_SCANNER_BUFFER_SIZE 未設定時の 1 行上限 (issue #231)。
const defaultScannerBufferSize = 4 * 1024 * 1024

// newStreamScanner は maxLine bytes を 1 行の上限とする Scanner を返す。
// 上限を超えた行は ErrTooLong にせず読み捨てる (oversizeSkippingSplit 参照)。
// maxLine <= 0 (config 未設定のテスト等) の場合は defaultScannerBufferSize を使い、
// 設定が効いていないことに気付けるよう Warn ログを出す (issue #310)。
func newStreamScanner(r io.Reader, maxLine int) *bufio.Scanner {
	if maxLine <= 0 {
		slog.Warn("runner: scanner buffer size is not positive, using default (issue #310)",
			"scanner_buffer_size", maxLine, "default", defaultScannerBufferSize)
		maxLine = defaultScannerBufferSize
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, maxLine), maxLine)
	scanner.Split(oversizeSkippingSplit(maxLine))
	return scanner
}

// oversizeSkippingSplit は bufio.ScanLines と同じく行単位で区切る SplitFunc。
// 改行が来ないままバッファ (maxLine) が一杯になったら、その行の残りを改行まで読み捨て、
// 行サイズとイベント種別を Warn ログに出す。
//
// 読み捨てた行が result イベントの場合は、readUntilResult が result を待ち続けて
// 止まらないよう、type / subtype / is_error だけを持つ result を代わりに返す
// (result 本文と usage は失われる)。それ以外の種別は何も返さない。
func oversizeSkippingSplit(maxLine int) bufio.SplitFunc {
	skipping := false
	skipped := 0
	var head []byte
	var sniffer *topLevelSniffer

	skip := func(chunk []byte) {
		skipped += len(chunk)
		sniffer.write(chunk)
	}

	finish := func() []byte {
		skipping = false
		evType := sniffer.fields["type"]
		subtype := sniffer.fields["subtype"]
		slog.Warn("runner: stream-json line exceeds scanner buffer, skipped (issue #299)",
			"event_type", evType,
			"subtype", subtype,
			"line_bytes", skipped,
			"scanner_buffer_size", maxLine,
			"head", string(head),
		)
		isError := sniffer.fields["is_error"] == "true"
		head, sniffer = nil, nil
		if evType != "result" {
			return nil
		}
		token, err := json.Marshal(map[string]any{
			"type":     "result",
			"subtype":  subtype,
			"is_error": isError,
			"result":   fmt.Sprintf("(bridge: result line exceeded scanner buffer: %d bytes)", skipped),
		})
		if err != nil {
			return nil
		}
		return token
	}

	return func(data []byte, atEOF bool) (int, []byte, error) {
		if skipping {
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				skip(data[:i])
				skipped++ // 改行
				return i + 1, finish(), nil
			}
			skip(data)
			if atEOF {
				return len(data), finish(), nil
			}
			return len(data), nil, nil
		}

		advance, token, err := bufio.ScanLines(data, atEOF)
		if advance > 0 || token != nil || err != nil {
			return advance, token, err
		}
		if len(data) >= maxLine {
			skipping = true
			skipped = 0
			head = append([]byte(nil), data[:min(len(data), oversizeHeadBytes)]...)
			sniffer = newTopLevelSniffer("type", "subtype", "is_error")
			skip(data)
			return len(data), nil, nil
		}
		return 0, nil, nil
	}
}

// topLevelSniffer は JSON object を分割して流し込み、トップレベルの指定キーの
// 短い値 (文字列 / true・false 等のリテラル) だけを拾う。行全体を保持しない。
// claude の stream-json はキー順が一定でなく (result の "type" は本文の後に来る)、
// ネストした "type" もあるため、行頭の正規表現ではなくこの方法で判定する。
type topLevelSniffer struct {
	want     map[string]bool
	fields   map[string]string
	depth    int
	inStr    bool
	esc      bool
	isKey    bool // 次の depth 1 の文字列がキー
	key      string
	buf      []byte
	overflow bool
}

// topLevelSnifferMaxValue は拾う値の最大長。これを超える値は捨てる。
const topLevelSnifferMaxValue = 64

func newTopLevelSniffer(keys ...string) *topLevelSniffer {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	return &topLevelSniffer{want: want, fields: map[string]string{}}
}

func (s *topLevelSniffer) write(p []byte) {
	for _, c := range p {
		if s.inStr {
			switch {
			case s.esc:
				s.esc = false
				s.add(c)
			case c == '\\':
				s.esc = true
				s.add(c)
			case c == '"':
				s.inStr = false
				s.endString()
			default:
				s.add(c)
			}
			continue
		}
		switch c {
		case '"':
			s.inStr = true
			s.resetBuf()
		case '{', '[':
			s.depth++
			if s.depth == 1 {
				s.isKey = true
			}
		case '}', ']':
			if s.depth == 1 {
				s.endLiteral()
			}
			s.depth--
		case ':':
			if s.depth == 1 {
				s.isKey = false
				s.resetBuf()
			}
		case ',':
			if s.depth == 1 {
				s.endLiteral()
				s.isKey = true
			}
		case ' ', '\t', '\r', '\n':
		default:
			if s.depth == 1 && !s.isKey {
				s.add(c)
			}
		}
	}
}

func (s *topLevelSniffer) add(c byte) {
	if s.depth != 1 {
		return
	}
	if len(s.buf) >= topLevelSnifferMaxValue {
		s.overflow = true
		return
	}
	s.buf = append(s.buf, c)
}

func (s *topLevelSniffer) resetBuf() {
	s.buf = s.buf[:0]
	s.overflow = false
}

func (s *topLevelSniffer) endString() {
	if s.depth != 1 {
		return
	}
	if s.isKey {
		s.key = ""
		if !s.overflow {
			s.key = string(s.buf)
		}
		return
	}
	s.record()
}

func (s *topLevelSniffer) endLiteral() {
	if len(s.buf) > 0 {
		s.record()
	}
}

func (s *topLevelSniffer) record() {
	if s.want[s.key] && !s.overflow {
		s.fields[s.key] = string(s.buf)
	}
	s.key = ""
	s.resetBuf()
}
