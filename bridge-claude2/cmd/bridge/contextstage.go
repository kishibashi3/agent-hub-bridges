// contextstage.go — 文脈の段を選ぶ env の受け口 (issue #339、epic #338 段 1)
//
// 役を claude2 ⇄ codex2 で載せ替えても文脈の単位が同じになるよう、両 bridge で同じ env 名・
// 同じ値で段を選ぶ。claude2 は 1 通ごとに文脈ゼロ (buildArgs に --resume が無く、stdin の
// session_id だけでは CLI は会話を復元しない。issue #406 で実機確認) で、none / last では
// env を受けて起動ログに出すだけで振る舞いは変えない。
//
//	none    段 1: 継続しない (既定)
//	last    bridge-codex2 の旧挙動 (`codex exec resume --last`、deprecated)。claude2 には
//	        相当する挙動が無いので warning を 1 行出して段 1 で動く
//	thread  段 2 (issue #340): 1 通ごとに文脈ゼロのまま、同じ鎖 (caused_by) の前の便を
//	        hub の get_thread で引いて prompt に添える (threadctx.go)
//
// 知らない値は起動エラーにする。黙って段 1 にすると、後続の段の値を指定したつもりで
// 段 1 が動くサイレント縮退になるため。
package main

import (
	"fmt"
	"log/slog"
	"os"
)

// contextStageEnv は文脈の段を選ぶ env 名 (bridge-codex2 と同じ)。
const contextStageEnv = "AGENT_HUB_CONTEXT_STAGE"

// contextStage は文脈の段。値は bridge-codex2 と揃える。
type contextStage string

const (
	// contextStageNone は段 1 (継続しない)。既定値。
	contextStageNone contextStage = "none"
	// contextStageLast は bridge-codex2 の旧挙動 (resume --last)。deprecated。
	contextStageLast contextStage = "last"
	// contextStageThread は段 2 (同じ鎖の前の便を hub から引いて添える、issue #340、threadctx.go)。
	contextStageThread contextStage = "thread"
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
	case contextStageNone, contextStageLast, contextStageThread:
		return s, nil
	default:
		return "", fmt.Errorf("%s %q: must be one of %q, %q, %q", contextStageEnv, v,
			contextStageNone, contextStageLast, contextStageThread)
	}
}

// logContextStage は選ばれた段を起動ログに 1 行出す。claude2 は last 相当の挙動を持たないので、
// last が指定されたときはその旨の warning を 1 行足して段 1 で動く。thread なら添える量の上限も出す。
func logContextStage(requested contextStage, threadCfg threadContextConfig) {
	if requested == contextStageLast {
		slog.Warn("context stage: "+string(contextStageLast)+" is a deprecated bridge-codex2 mode with no "+
			"equivalent in "+bridgeType+" — running as "+string(contextStageNone)+" (issue #339)",
			"env", contextStageEnv)
	}
	slog.Info("context stage selected",
		"stage", string(effectiveContextStage(requested)), "requested", string(requested), "env", contextStageEnv)
	if requested == contextStageThread {
		logThreadContextConfig(threadCfg)
	}
}

// effectiveContextStage は claude2 が実際に動く段を返す。last は段 1 (none) に読み替える。
func effectiveContextStage(requested contextStage) contextStage {
	if requested == contextStageThread {
		return contextStageThread
	}
	return contextStageNone
}
