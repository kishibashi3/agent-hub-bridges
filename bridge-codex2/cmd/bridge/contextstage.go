// contextstage.go — 文脈の段を選ぶ env (issue #339、epic #338 段 1)
//
// 役を claude2 ⇄ codex2 で載せ替えても文脈の単位が同じになるよう、両 bridge で同じ env 名・
// 同じ値で段を選ぶ。
//
//	none  段 1: 継続しない (既定)。1 便ごとに `codex exec` で新しいセッションを始める
//	last  旧挙動 (deprecated): `codex exec resume --last` で全員の便を 1 本のセッションに積む。
//	      別の送り主・別の仕事の作業が混ざる。段階的 deprecation のため env で残し、
//	      起動時に warning を 1 行出す
//
// env を変えて再起動するだけで切り替わる (rebuild 不要)。知らない値は起動エラーにする。
// 黙って段 1 にすると、後続の段の値を指定したつもりで段 1 が動くサイレント縮退になるため。
package main

import (
	"fmt"
	"log/slog"
	"os"
)

// contextStageEnv は文脈の段を選ぶ env 名 (bridge-claude2 と同じ)。
const contextStageEnv = "AGENT_HUB_CONTEXT_STAGE"

// contextStage は文脈の段。値は bridge-claude2 と揃える。
type contextStage string

const (
	// contextStageNone は段 1 (継続しない)。既定値。
	contextStageNone contextStage = "none"
	// contextStageLast は旧挙動 (resume --last)。deprecated。
	contextStageLast contextStage = "last"
)

// defaultContextStage は env 未指定時の段。
const defaultContextStage = contextStageNone

// resolveContextStage は AGENT_HUB_CONTEXT_STAGE env を解決する。未指定・空なら既定値。
func resolveContextStage() (contextStage, error) {
	v := os.Getenv(contextStageEnv)
	if v == "" {
		return defaultContextStage, nil
	}
	switch s := contextStage(v); s {
	case contextStageNone, contextStageLast:
		return s, nil
	default:
		return "", fmt.Errorf("%s %q: must be one of %q, %q", contextStageEnv, v, contextStageNone, contextStageLast)
	}
}

// logContextStage は選ばれた段を起動ログに 1 行出す。last のときは deprecation warning を 1 行足す。
func logContextStage(stage contextStage) {
	if stage == contextStageLast {
		slog.Warn("context stage: "+string(contextStageLast)+" (codex exec resume --last) is deprecated — "+
			"all senders share one codex session; unset "+contextStageEnv+" or set it to "+
			string(contextStageNone)+" (issue #339)",
			"env", contextStageEnv)
	}
	slog.Info("context stage selected", "stage", string(stage), "env", contextStageEnv)
}
