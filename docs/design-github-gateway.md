# GitHub ゲートウェイ peer 設計（issue #407）

GitHub への書き込みを 1 つの非エージェント peer に通し、**DM の送り主の役（role）と、書き込み先の owner（org / user）で、許す操作と使う鍵を切り替える**。

- 状態: **設計のみ。実装はしていない。**
- 正本の依頼: kishibashi3/agent-hub-bridges#407（制約の追加: [権限表のメンテ](https://github.com/kishibashi3/agent-hub-bridges/issues/407#issuecomment-7116121242) / [App を使う・使わないの両方](https://github.com/kishibashi3/agent-hub-bridges/issues/407#issuecomment-7116190995) / [範囲に ntv-contech を入れる・複雑にしない](https://github.com/kishibashi3/agent-hub-bridges/issues/407#issuecomment-7189759185)）
- **2026-11-26 の見直し**: 範囲に `ntv-contech` を入れ（旧 §6 S4 の「範囲外」を取り消し）、最初の段（段 1）を下の 3 つの目標に絞った。役ごとの鍵・表のメンテの機械提案・inbound・読み取りトークン（R1）は後の段（段 2）に回した（§6.1）
- **#73 は「どれを採るか」ではなくなった。** 鍵の供給は App でも PAT でも動く構成にする（§7）。#73 の答えで決まるのは「App を使う構成のときの App の数・権限・入れ先」だけ
- 時刻は RFC 3339 UTC で書く

---

## 0. 結論（先に）

**段 1 の目標（kishibashi3 の要件、2026-11-26）:**

| | 目標 | 何で止めるか |
|---|---|---|
| ① | NTV 以外の peer が `ntv-contech/*` に書き込めない | 表: peer ごとに書いてよい owner を持つ（§3.2）。鍵: `kishibashi3` の token は `ntv-contech` の repo を見られない（§7.3） |
| ② | NTV の peer が `kishibashi3/*` に書き込めない | 表: 同上。鍵: `k-ishibashi-ntv` の token は `kishibashi3` の repo に READ しか持たない（§7.3） |
| ③ | `ntv-contech/frontend` に、表で許していない操作をできない | 表だけ: repo ごとの op の上限（§3.2）。鍵は repo も操作も絞れない |

- **鍵はいまある token 2 本で始める。owner ごとに 1 本**（`kishibashi3` → `kishibashi3/*`、`k-ishibashi-ntv` → `ntv-contech/*`）。どちらも OAuth token（scope `repo` を含む）で fine-grained ではないので、**操作の制限は表だけが持つ**（§7.1 の C0）。owner を足すのは表と鍵の設定に 1 行ずつ足すだけにする（§3.2）
- **①〜③が本当に止まるのは S3（peer から書き込みの資格情報を外す）以降。** S1・S2 の間は、どの peer も `~/.config/gh/hosts.yml` の token 2 本を自分で使えるので、ゲートウェイの判定は**助言・警告**にすぎない。S3 の前提 #409（peer と鍵を別の uid に分ける）の担当は別途決める（§6）
- **S3 の後も、送り主の名乗り替え（§1.3）が残る間は①②は破れる。** NTV 以外の peer が NTV の peer の handle を名乗れば、ゲートウェイはその handle の owner で通してしまう。塞ぐのは hub 側の handle ごとの資格情報（#391 / #409、§5.1）

以下は段 1・段 2 を通した設計の全体。

1. **非エージェント peer `@github`（仮称）を新設する。** `@scheduler` と同じく、DM の `/` コマンドを決まった形でだけ受け、LLM を通さない。送り主の handle から**役（role）**を引き、**その役に許された操作だけ**を、**その役の鍵**で実行する。それ以外は断る
2. **鍵の供給は差し替えられる。** GitHub App の installation token でも、PAT でも動く（§7）。**App あり・App なしのどちらも本線**として扱う（顧客環境では App が使えないことがある）。**DM の語彙・断り方・監査の記録の形は鍵の種類で変えない。** 違いは「落ちるもの」の表（§7.2）に出す
3. **権限表は peer 単位ではなく役の種類で束ねる**（§3.2）。新しい peer は既存の役と書いてよい owner を 1 行で割り当てるだけにする（ただし `members` に 1 行足すことも**広げる変更**で、kishibashi3 本人の merge が要る。§3.8）。表は既定拒否。**段 2 で**、足す提案と減らす提案の両方をゲートウェイが監査から機械的に出し、人は yes / no だけを決める形にする（§3.8）
4. **inbound（`@gh-watch`）とはプロセスを分ける**（段 2）。inbound は App の webhook でも repo の webhook でも動く（§4）
5. **ゲートウェイを入れただけでは何も守れない。** 各 peer が `gh` と共有トークンを持ったままなら、ゲートウェイは助言にすぎない。**外す段（S3）と、その前提になる「peer と鍵を別の uid に分ける」ことを段の中に入れた**（§6）。uid を分ける作業は #409（担当未定）で、S3 はそれが終わるまで始められない
6. **「絶対に事故が起きない」とはならない。** 残るもの: ゲートウェイの設定誤り／相手と鍵の対応の誤り／迂回。さらに、**今の hub では送り主の名乗りを偽れる**（§1.3）。これは「対応の誤り」と「迂回」の両方につながる。残るものは §5 にまとめた
7. **人・CI・ゲートウェイを通らない peer との競合は守れない**（§5.2）
8. **ゲートウェイが詰まり所になる。** 落ちると `kishibashi3/*` と `ntv-contech/*` への peer の書き込みが全部止まる（§5.3）

---

## 1. 現状の問題

### 1.1 書き込みの経路

観測: この host（bridge host）の作業ツリーとコード、#73・#391・#398 の operator / planner の観測。2026-11-24 時点。

- 全 bridge の内側プロセスが `gh` を使える。認証は `~/.config/gh/hosts.yml` の `kishibashi3` トークン。scope は `admin:public_key` `delete_repo` `gist` `read:org` `repo` `project`（#398）。**どの peer も、どの repo に対しても、merge・削除を含めて何でもできる**
- `hosts.yml` には **`k-ishibashi-ntv` の token も入っている**（2026-11-26 に `gh auth status` で確かめた。scope `gist` `read:org` `repo` `workflow`）。`ntv-contech/*` を触る peer はこちらを使う。2 本とも同じファイルにあるので、どの peer も両方の owner に書ける
- git の push も `gh` を credential helper にしている環境では同じトークンを通る（#73 の 2026-10-30 コメント、論点 C）
- 全 peer が同じアカウントなので、GitHub 上の author は全部 `kishibashi3`。人と AI の区別も、どの peer かの区別も GitHub 側に残らない。role は footer の文字でしか分からない
- 作者が自分の PR を approve できないため、formal な approve が使えず `LGTM ✅` コメントで代用している
- **`gh` の設定（どのアカウントが active か）が全 peer で共有されている。** 1 つの session が `gh auth switch` すると、他の peer の書き込み先の名義も変わる。**この設計を書いている最中に実際に起きた**（2026-11-24）: operator の session が `gh` の active アカウントを切り替えて元に戻した結果、active が `k-ishibashi-ntv` になっており、`kishibashi3` の repo への操作が通らなかった。`GH_TOKEN=$(gh auth token -u kishibashi3)` をコマンドごとに付けて、共有設定は書き換えずに回避した。**peer ごとに鍵を持たせず、鍵の選択をゲートウェイに集める**のはこれをなくすためでもある（#391 も同じ問題を直す対象にしている）

### 1.2 既にあるもの

| もの | 中身 | 本設計での使い方 |
|---|---|---|
| `packages/github-client/iat.go` | App の JWT を作り、installation token を取り、期限 5 分前に取り直す。**権限と repo を絞る引数は無い**（`POST /app/installations/{id}/access_tokens` を body なしで呼ぶ） | ゲートウェイの鍵の部品。絞る引数（§3.3）を足す必要がある |
| `bridge-claude2` の IAT モード（#228） | `GITHUB_APP_*` が揃うと内側プロセスに `GH_TOKEN=<IAT>` を注入する。`GITHUB_APP_*` 自体は内側に渡さない（`filteredEnv`） | ゲートウェイ導入後は使わない方向（§6 S3）。**IAT を内側に渡すと、その 1 時間は App の全権限が peer の手にある** |
| `packages/gh-watch` | webhook を HMAC 検証して受け、購読に合えば DM を送る。購読は sender 単位（`/watch` `/list` `/delete`）。SQLite。`GH_WATCH_WEBHOOK_SECRET` が無いと起動しない | inbound として残す（§4） |
| `agent-hub/packages/scheduler` | 非 LLM peer。`/` コマンドを受け、entry を `owner`（= 送り主）単位で持ち、自分の entry だけ操作させる | ゲートウェイのコマンド受付・送り主単位の制限の形の手本 |

`gh-watch` は動かしていない（webhook secret と公開 ingress が無い）。コードを読んだ範囲での記述。

### 1.3 送り主の名乗りは偽れる（今の hub）

ゲートウェイは「DM の `from`」で鍵を選ぶ。その `from` の確かさは hub の認証で決まる。今の hub は:

- PAT を検証して GitHub login（`kishibashi3`）を得る
- handle は `X-Participant-Id` ヘッダの値をそのまま使う（`agent-hub/src/mcp/server.ts` の auth middleware）。login と違う handle で接続すると WARN ログが出るだけで通る
- `AGENT_HUB_STRICT_HANDLE_OWNERSHIP` を立てると login と違う handle を拒否するが、**全 peer が同じ PAT なので、立てると全 peer が `@kishibashi3` になり fleet が動かない**

そして hub 接続用の `GITHUB_PAT` は bridge の内側プロセスの env に入っている（`bridge-claude2` の `filteredEnv` は `GITHUB_APP_*` しか除かない。`bridge-codex2` は `GITHUB_PAT` を明示的に渡している）。

→ **どの peer の内側プロセスも、`X-Participant-Id: reviewer` で hub に繋ぎ、`@reviewer` を名乗って `@github` に DM できる。** ゲートウェイは `@reviewer` 用の鍵で実行してしまう。

これは本設計の中では塞げない。塞ぐには hub が handle ごとに別の資格情報を持つ必要がある（§8 の #391 / #409）。それまでの間、ゲートウェイの「相手ごとの切り替え」は**事故（peer が間違った操作を頼む）には効くが、意図的な・あるいは LLM が試行錯誤で行う名乗り替えには効かない**。§5.1 に残るものとして書く。

---

## 2. 案の全体

```
  peer (LLM)                 @github (非 LLM)                        GitHub
  ───────────               ──────────────────                      ──────
  DM: /pr comment ...  ──▶  1. from を読む
                            2. policy.yaml で from → (役, owner) → (op, repo) を引く
                               無ければ 拒否 → DM で返す + 監査に記録
                            3. 鍵を鍵の供給元から受け取る（段 1 は owner の鍵）
                               App: repo と op に要る権限だけに絞って
                                    installation token を作る          ──▶ access_tokens
                               PAT: 事前に用意した token を使う（段 1 は owner ごと）
                            4. 対象ごとの直列キューに入れて実行        ──▶ REST API
                            5. 結果を監査に記録 → DM で返す
  ◀── 結果 DM (caused_by)
```

- 鍵（App の秘密鍵、または PAT）はゲートウェイの uid だけが読める場所に置く。**peer には秘密鍵も書き込み用トークンも渡さない**
- 3 の「鍵の供給元」だけが鍵の種類で変わる。1・2・4・5（受け付け・判定・断り方・実行・記録）は鍵の種類を知らない（§7）
- 読み取りは別扱い（§3.6）

---

## 3. 設計してほしいこと 1〜2・5: 鍵と操作の決め方、断り方、監査

### 3.1 コマンドの形

`@scheduler` と同じく 1 行目が `/` コマンド、2 行目以降が本文（コメント本文など）。決まった形以外は全部断る（推測で解釈しない）。

```
/issue comment kishibashi3/agent-hub-bridges#407
<本文>

/pr review kishibashi3/agent-hub-bridges#410 approve sha=<40 桁>
<本文>

/pr merge kishibashi3/agent-hub-bridges#410 sha=<40 桁> method=squash
```

最初に入れる操作の候補（peer が実際に使っている操作の集計は §11。役ごとの表の中身はその集計を根拠に決める。まだ決めていない）:

| op | 対象 | 必須引数 | 備考 |
|---|---|---|---|
| `issue.create` | repo | title, 本文 | |
| `issue.comment` | issue / PR 番号 | 本文 | |
| `issue.close` / `issue.reopen` | issue 番号 | | |
| `issue.label` | issue / PR 番号 | 追加・削除するラベル | |
| `pr.create` | repo | head, base, title, 本文 | head の branch は先に push されている必要がある（§3.7） |
| `pr.comment` | PR 番号 | 本文 | |
| `pr.review` | PR 番号 | `approve` / `request_changes` / `comment`, **sha** | API の `commit_id` に sha を渡す。見た head 以外への review にならない |
| `pr.merge` | PR 番号 | **sha**, method | API の `sha` に渡す。head が変わっていれば GitHub が 409 で断る |
| `push` | repo, branch | 下の §3.7 | |
| `token.read` | — | — | **段 2。** 読み取りトークン（§3.6 R1）を送り主の受け取り場所に書く。**値は DM に入れない** |

対象の書き方は `<owner>/<repo>#<n>` に固定する（owner を省略した形は受けない）。owner で鍵と許可が決まるため（§3.2・§3.3）。

役によらず許すコマンド（GitHub に何も書かず、表も変えない）: `/ping`、`/propose`（**段 2**。§3.8。提案 PR はゲートウェイ自身の鍵で出し、表は人が merge するまで変わらない。1 日 1 回まで）。**この 2 つ以外は、送り主が `members` に無ければ全部 `no-role` で断る。** `@scheduler` には役を付けない。付けると `/run_in … @github /pr merge …` で、誰でも `@scheduler` の役で実行させられる

ゲートウェイは本文の末尾に footer を**自分で**付ける（`@<from> via @github`）。peer が書いた footer は信用しない。本文の中の footer に似た行（`@<handle> [` で始まる行や `via @github` を含む行）は、ゲートウェイが除いてから投稿する。**`LGTM ✅` の判定もゲートウェイの footer で数える**（`@reviewer via @github` の footer があるコメントだけを LGTM とする）。`impl` の役も `pr.comment` を持つので、本文に `LGTM ✅ … @reviewer [` と書けば今の判定をすり抜けられるため。planner の判定をこの形に切り替えるのは S2 で行う

### 3.2 ポリシー表（役の種類で束ねる）

**行は peer ではなく役の種類で作る。** peer は役に割り当てるだけ。

```yaml
version: 2

# 書き込み先の owner（org / user）。owner を足すときはここに 1 行と、鍵の設定（§7.1）に 1 行
owners:
  kishibashi3: {}
  ntv-contech: {}
  # 表を置く専用 repo（§3.8）の owner も、その repo への書き込みはコードで断る

# 役の種類。許す op の組だけを持つ。peer の名前も owner も出てこない
roles:
  review: { ops: [pr.review, pr.comment, issue.comment] }
  impl:   { ops: [issue.create, issue.comment, pr.create, pr.comment, push],
            push: { branch_prefixes: ["feat/", "fix/", "docs/", "test/", "refactor/"] } }   # §3.7
  plan:   { ops: [pr.merge, issue.close, issue.create, issue.comment, pr.comment, issue.label] }

# peer → 役と、書いてよい owner。1 peer に役は 1 つ
members:
  "@reviewer":           { role: review, owners: [kishibashi3] }
  "@planner":            { role: plan,   owners: [kishibashi3] }
  "@bridges-impl":       { role: impl,   owners: [kishibashi3] }
  "@ntv-reviewer":       { role: review, owners: [ntv-contech] }
  "@ntv-planner":        { role: plan,   owners: [ntv-contech] }
  "@ntv-frontend-coder": { role: impl,   owners: [ntv-contech] }

# repo ごとの op の上限。役の ops との積をとる（狭めるだけで、広げられない）
repos:
  ntv-contech/frontend: { ops: [issue.comment, pr.comment, pr.create, pr.review, push] }   # 例。中身は §10.2 の 3
```

**判定**（全部通ったときだけ実行する。既定拒否）:

1. 送り主が `members` にある。無ければ `no-role`
2. 対象 repo の owner が `owners` にあり、かつ送り主の `owners` に入っている。無ければ `owner-not-allowed`（目標 ①②）
3. op が送り主の役の `ops` にある。無ければ `op-not-in-role`
4. 対象 repo が `repos` にあれば、op がその `ops` にもある。無ければ `op-not-in-repo`（目標 ③）

- **owner を足すのは設定の追加だけ**: `owners` に 1 行、鍵の設定（§7.1）に 1 行、その owner を触る peer の `owners` に足す。役も判定のコードも変えない。どれも**広げる変更**（§3.8）
- **`repos` は狭めるだけ。** 役が許していない op を `repos` に書いても通らない（積をとるため）。`repos` に無い repo は役の `ops` がそのまま上限になる
- 1 peer が 2 つの owner を触る（`owners: [kishibashi3, ntv-contech]`）ことは表の形としては書ける。**書くと①②の例外になる**ので、そういう peer を作るかは kishibashi3 が決める（広げる変更）。いまの fleet ではどちらか片方を触る peer だけを想定している
- 旧版の `repo_groups` はやめた。repo の束は owner で足りる（owner の中で特定の repo を狭めたいときは `repos`）
- **1 peer に役は 1 つ。** 2 つの役の op が要る peer が出たら、両方を含む役を作る（例の `plan`）。1 peer に複数の役を許すと、どの鍵を使うかが曖昧になる（1 便目の「2 件以上当たったら設定誤り」はこの制約で起きなくなる）
- **rule に peer 名を書く例外は作らない。** 1 体だけに要る op は、その 1 体のために役を 1 つ作る（それが例外だと表の上で見える）
- `push` を持つ役は `push.branch_prefixes` が必須（無ければ起動時の検証で落とす）
- 起動時に検証する（未知の op 名・役・owner、owner に鍵が無い、鍵ファイルの mode が 600 でない、等）。**1 つでも通らなければ起動しない**（中途半端な表で動かない）
- 誰にどの op を許すかは本 doc では決めない。上の例は形の説明。**誰が何をしてよいかは L1 の判断**（`collaboration-model` の L0/L1/L2 に合わせる）

**判定軸: 新しい peer を足すときに `members` に 1 行足すだけで済むか。** 済む（その 1 行も広げる変更なので、PR にして kishibashi3 本人が merge する。§3.8）。`roles` の行の数は「役の種類 × 1」で、peer の数（今 29 体）にも repo の数にも比例しない。`owners` は owner の数、`repos` は狭めたい repo の数だけ。peer ごとの行にしなかった理由:

| | peer 単位 | 役の種類で束ねる |
|---|---|---|
| 表の大きさ | peer 数 × op × repo | 役の数 × op（repo は owner で束ね、狭めたい repo だけ `repos` に書く） |
| peer を足す | 行を書き起こす（何を許すかをその都度決める） | 既存の役に 1 行で割り当てる |
| 減らす提案（§3.8） | peer ごとに「使っていない」が出る。新しい peer は全部候補になる | 役の中の誰も使っていないものだけが出る |
| 鍵の数（§7.3） | peer 数に比例 | 段 1 は owner の数（2）。段 2 で役ごとに分けると役の種類 × owner |
| 失うもの | — | 同じ役の peer の間で差を付けられない。付けたいときは役を分ける |

### 3.3 鍵の選び方

**段 1: 鍵は書き込み先の owner で決まる**（owner ごとに 1 本。§7.1 の C0）。判定（§3.2）を通った依頼だけが、対象 repo の owner の鍵で実行される。役は鍵の選択に関わらない。

段 2 以降で役ごとに鍵を分ける場合（§7.3）、鍵は (役, owner) で決まる。どう作るかは鍵の供給元（§7）が持つ:

- **App の供給元**: installation token を作るときに `repositories` と `permissions` を指定して、依頼 1 件分（その repo・その op に要る権限）まで絞る（`POST /app/installations/{id}/access_tokens` の body）。例: `issue.comment` を `agent-hub-bridges` に → `{"repositories":["agent-hub-bridges"],"permissions":{"issues":"write"}}`。トークンは (App, repo, 権限の組) ごとにキャッシュし、期限 5 分前に作り直す（今の `iat.go` と同じ考え方。`iat.go` に絞る引数を足す必要がある）
- **PAT の供給元**: 事前に作った PAT を使う。依頼ごとには絞れない（§7.3）。fine-grained なら鍵に付けた権限・repo、classic / OAuth token（段 1 の C0）なら scope と持ち主のアカウントが触れる repo 全部が上限になる

どちらでも、**op を許すかどうかの判定はゲートウェイの表で行う。** 鍵の権限は「表が誤っていたとき・ゲートウェイに不具合があったときの上限」の役目になる。App のほうがその上限が細かい、というのが違い（§7.2）。段 1 の C0 では、鍵の上限が効くのは **owner をまたぐ書き込み（目標 ①②）だけ**で、owner の中の repo・op（目標 ③）には効かない（§7.3）。

### 3.4 断り方

- 送り主に DM で返す（`caused_by` = 依頼 msg ID）。形: `❌ denied: <op> <owner>/<repo>#<n> — <理由>（no-role / owner-not-allowed / op-not-in-role / op-not-in-repo / bad-args / expired / same-identity / key-unavailable）`
- **理由の語彙は鍵の種類で変えない。** 例えば `same-identity`（PR の作者と同じ名義では approve できない）は、名義が 1 つの構成で多く出るが、App が 2 つの構成でも同じ語で返す。`key-unavailable`（鍵が取れない: App の秘密鍵の不備、installation が無い、PAT の期限切れ）も同じ。GitHub の応答に App / PAT 固有の文言が出ても、送り主には上の語彙に直して返し、原文は監査に残す
- 理由は送り主が直せる粒度で書く。他の peer の rule の中身は書かない
- **拒否したときに別の鍵で試すことはしない。** 鍵の供給元は起動時に 1 つに決まり、動いている間に App から PAT へ落とすことはしない（IAT モードの「未設定なら PAT」のような逃げ道はゲートウェイには作らない）
- 拒否も監査に記録する（§3.5）。拒否が続く送り主は、表の不足か、peer が誤った操作を頼んでいるかのどちらか。operator が見る

### 3.5 監査の記録

SQLite（ゲートウェイの uid だけが書ける。**#409 で uid を分けるまでは全 peer が同じ uid なので、peer から書き換えられる**。§5.1 の迂回に入れた）。1 依頼 1 行、状態を更新していく。

| 列 | 中身 |
|---|---|
| `msg_id` | 依頼 DM の ID（**一意**。同じ DM を 2 回実行しないための鍵） |
| `caused_by` | 依頼 DM の caused_by（因果の連鎖） |
| `received_at` / `started_at` / `finished_at` | RFC 3339 UTC |
| `from` | 送り主 handle（§1.3 のとおり、hub の名乗りをそのまま記録したもの） |
| `op` / `owner` / `repo` / `target` | 何を・どこに（`owner` は鍵の選択と目標 ①② の集計に使う） |
| `args_digest` | 本文の sha256 と長さ（本文は GitHub 側に残るので全文は持たない） |
| `decision` | `allowed` / `denied` |
| `deny_reason` | §3.4 の語彙 |
| `role` | 送り主に当たった役 |
| `policy_sha` | 判定に使った表の commit sha（§3.8 の反映） |
| `key_id` / `token_scope` | 使った鍵（段 1 は `pat:kishibashi3` / `pat:ntv-contech` のような owner の ID。段 2 は `app:bot` / `pat:impl@kishibashi3` など。値は持たない）と、絞った権限・repo（PAT のときは鍵に付けた上限をそのまま書く） |
| `state` | `queued` → `running` → `done` / `failed` / `unknown` |
| `http_status` / `github_request_id` | GitHub の応答（`X-GitHub-Request-Id`） |
| `result_url` | できたコメント・PR・merge commit の URL |

- `running` のまま落ちた行は、再起動時に**自動で再実行しない**。`unknown` にして送り主に「結果不明、GitHub で確かめてから再依頼して」と返す（コメントの二重投稿・二重 merge を避ける）
- 列は鍵の種類で変えない。違いは `key_id` / `token_scope` の値だけ
- GitHub 側の記録: author は鍵の名義（App なら bot、PAT なら PAT の持ち主）、footer に `@<from> via @github` が入る。「誰が・いつ・どこに・何をした」は GitHub 側の author + footer と、ゲートウェイの監査の 2 か所で突き合わせられる。**名義が 1 つの構成では GitHub の author だけでは peer を区別できないので、監査の `result_url` との突き合わせが主になる**（§9）

### 3.6 読み取り

> **段 2。** 段 1（S1・S2）では peer は今の `gh` を持ったままなので、読み取りは今のまま。段 1 の C0 の鍵は OAuth token（classic と同じく `repo` scope が読み書き両方）なので、そもそも読み取り専用の鍵が作れない。R1 に要る読み取り専用の鍵を作るのは段 2 に回した。①〜③は書き込みの目標なので、R1 が無くても成り立つ。ただし **S3 で書き込みの資格情報を外すと読み取りも一緒に消える**ので、S3 に入る前に R1 の鍵（段 2）か R2 のどちらかが要る（§6・§10.2 の 12）

読み取り（`gh pr view` / `gh pr diff` / `gh pr checks` / `gh api` の GET）は peer の作業のほとんどを占める（未集計、S1 で測る）。これを全部ゲートウェイに通すと詰まり所が大きくなるので、2 案:

- **R1（採る）**: ゲートウェイが**読み取り専用に絞ったトークン**（contents / pull_requests / issues / checks / actions すべて `read`、repo も絞る）を peer に渡す。peer は今の `gh` で読める。漏れても書けない
  - App の構成: ゲートウェイが 1 時間のトークンを作って渡す
  - PAT の構成: 読み取り専用の fine-grained PAT を事前に 1 本作り、ゲートウェイが渡す。**期限が PAT の期限（最長 366 日 か 無期限）になる**ので、漏れたときに読める期間が長い（§7.2）。classic PAT には読み取りだけの scope が無い（`repo` が読み書き両方）ので、classic しか使えない環境では R1 は成り立たず R2 になる
- R2: 読み取りもゲートウェイに通す。記録は全部残るが、待ちが増える

**R1 を採る**（§10.1）。読み取りは peer の `gh` 呼び出しの 9 割を占める（§11）ので、R2 では詰まり所が大きすぎる。可逆なので、R1 で漏れが問題になれば R2 に戻せる。

**渡し方（DM では渡さない）**: ゲートウェイの窓口は hub の DM なので、DM で渡すとトークンの値が hub の履歴と dashboard に残る。PAT の構成ではその値が最長 366 日か無期限で有効になる。そのため:

- ゲートウェイは、`token.read` を持つ役の peer ごとに**受け取りファイル**（例: `<peer の受け取り dir>/gh/hosts.yml`）にトークンを書く。peer の `gh` は `GH_CONFIG_DIR` をその dir にして読む（共有の `~/.config/gh` は使わない。§1.1 の `gh auth switch` の巻き込みもなくなる）
- 受け取り dir は、その peer の uid とゲートウェイの uid だけが読める（#409 で uid を分けた後。分けるまでは同じ uid なので、どの peer も他の peer の読み取りトークンを読める。読み取り専用なので、漏れたときの影響は「読める」だけ）
- App の構成ではゲートウェイが期限の前（例: 55 分ごと）に書き直す。PAT の構成では起動時に 1 回書き、PAT を入れ替えたときに書き直す
- DM の `/token read` は「書き直して」の依頼だけで、返信には値を入れない（`ok: written <時刻>` だけ返す）
- 未確認: `gh` が `hosts.yml` に書いた installation token / fine-grained PAT をそのまま使えるか（`GH_TOKEN` では使えることは今の IAT モードで確かめている）。S1 で 1 体で確かめる。使えなければ、bridge が spawn ごとにファイルから `GH_TOKEN` を読み込む形にする

App の構成の R1 は、読み取りトークンを作るのがゲートウェイなので、ゲートウェイが落ちると最長 1 時間後に読み取りも止まる（§5.3）。

### 3.7 push

push は working tree が要るので、DM 1 通では運べない。案:

- **P1**: peer が `git bundle` を作り、ゲートウェイが読める場所に置いてパスを DM する。ゲートウェイは bundle を検証して、自分の鍵で push する。**鍵は peer に渡らない**。検証（1 つでも通らなければ `bad-args` で断る。鍵の種類によらず同じ）:
  - `git bundle verify` が通る（前提の commit がゲートウェイの clone にある）
  - bundle の ref は **`refs/heads/<branch>` がちょうど 1 つ**。tag や 2 つ以上の ref は断る
  - branch 名が役の `push.branch_prefixes`（§3.2）のどれかで始まる。`main` などの保護 branch・既定 branch は prefix に関係なく断る
  - **fast-forward か新しい branch だけ**。force push は断る（push は `--force` を付けずに行い、GitHub が non-fast-forward で断ればそのまま `failed` で返す）
  - push する commit が `.github/workflows/` の下を変えていたら断る（workflow を変えると CI の中で鍵や secret に触れられるため）。workflow の変更は人の経路に残す。App の構成で Workflows: write が無い場合（§7.5）と同じ扱いを、PAT の構成でも取る
- P2: ゲートウェイが、その repo・`contents: write` だけに絞ったトークンを peer に渡す。単純だが、その 1 時間は peer がその repo に何でも push できる（**迂回の窓が開く**）。**PAT の構成では「その repo だけ・1 時間だけ」が作れない**ので、渡すのは事前に作った PAT そのものになり、窓が PAT の期限まで開く。PAT の構成では P2 は採らない

**P1 を採る**（鍵の種類によらず同じ。§10.1）。P1 は bundle をファイルで受け渡すので、ゲートウェイは peer と同じ host に置く（§6 S3 の前提）。P2 は App の構成で S2 の間の暫定として使うかもしれない（その場合は保護 branch を GitHub 側の ruleset で守る）。App の構成で push を App の名義で行えるかは #73 の権限（Contents: write）次第（§7.5）。

### 3.8 権限表のメンテ（足す・減らす・記録）

> **段 1 で入れるのは「表は専用 repo に置き、変更は PR、広げる変更の merge は kishibashi3 本人だけ」の記録の規則（この節の後半）だけ。** 監査からの機械的な提案（`/propose`、足す提案・減らす提案）は段 2 に回した。段 1 の表は小さく（役 3・owner 2・狭める repo 1）、人が読んで足し引きできる。提案が無くても判定は既定拒否のまま動くので、①〜③は成り立つ。失うのは「使われていない許可が残り続ける」のを機械が見つけることだけ

放っておくと表は足す方向にだけ動く。**使われていない許可は害が見えないので残り続ける**（前例: `@reviewer` の規則が 653 行まで伸び、自己改善の節 3 箇所が実行 0 回 —— `deep-research/research-archive/2026-11-23-reviewer-refactor-proposal.md`）。そのため、減らす経路を足す経路と同時に作る。

**材料は監査（§3.5）だけを使う。** ゲートウェイは通った依頼しか存在しない場所なので、「記録し忘れ」が起きない（`@reviewer` のリファクタ案 E は、subagent の起動 52 回のうち委任が archive に残っていたのが 10 件だった）。これは鍵の種類によらない。

| | 足す提案 | 減らす提案 |
|---|---|---|
| 根拠 | **断られた記録**: `denied` のうち `op-not-in-role`（`owner-not-allowed` と `op-not-in-repo` は目標 ①〜③ の拒否なので、足す提案の根拠にしない） | **使われていない記録**: 役に許された (op, owner) のうち、N 日 `allowed` が 0 件 |
| 単位 | (役, op, owner)。peer ごとには出さない | (役, op, owner)。役の中の誰か 1 体でも使っていれば出さない。`members` のうち N 日依頼が 0 件の peer も候補に出す |
| 出す条件 | 同じ (役, op, owner) の拒否が窓の中で K 件以上、**かつ異なる送り主が min(K, 役の members の数) 体以上**（1 体が K 回送っただけで表を広げないため）。**初期値 K = 3、窓 = 14 日** | **初期値 N = 60 日** |
| 付ける根拠 | 拒否した依頼の msg_id と送り主の一覧。peer の誤りか表の不足かを人が見分けられるように。**§1.3 が残る間は、PR 本文に「送り主は hub の名乗りのままで未検証」と書く**（1 体が複数の handle を名乗れば、送り主の数の条件も満たせるため） | 最後に使われた日時（無ければ「表に入ってから一度も」） |

初期値の根拠（§11 の集計）: peer の書き込みは 70 日で 650 件ほどで、役によっては正当な op が 70 日に 1〜2 回しか出ない（例: `@reviewer` の `issue create` 1 件、`@writer-ja` の `pr ready` 1 件）。N = 30 日だと、こうした op を毎月「減らす」に出してしまう。K は、1 体が同じ誤りを 1〜2 回繰り返しても広げる提案にならない値にした。**どちらも運用で直す値**で、直すときは表と同じく PR にする。

- **提案はゲートウェイが機械的に出し、PR にする。** 人は PR を merge する（yes）か close する（no）かだけを決める。表を人が書き起こすことはしない
- 提案を出す頻度は週 1（`@scheduler` から `/propose` を叩く）。同じ提案が close されたら、根拠が増えるまで（足す提案は拒否件数が倍になるまで、など）出し直さない
- 役を分ける・新しい役を作る、は機械の提案に含めない（人が「この 1 体だけ」と判断したときに書く PR）。その PR も下の記録の規則に従う

**表は専用 repo のファイルにする**（名前は未定。例: `kishibashi3/agent-hub-policy`）。roles-kaz には置かない: roles-kaz は main 直接 push の運用で「変更は PR として残る」と合わないうえ、表の repo への書き込みを全部断る（下）と、roles-kaz への全 peer の書き込みが止まる。専用 repo には ruleset で「main への変更は PR だけ」を掛ける。変更は PR として残り、誰がいつ広げたかが GitHub に記録される。root `CLAUDE.md` §「自分の権限範囲は自分で書き換えない」の「**緩める方向にだけ外部の独立記録を要求する**」形を当てる:

- **広げる変更**（役に op を足す、peer をより広い役に移す、役を足す、**`members` に peer を足す**、**`owners` に owner を足す・peer の `owners` に owner を足す**、**`repos` の上限を広げる・消す**）: merge するのは kishibashi3 本人だけ。`members` に足す PR は、その peer を spawn する operator が書く。merge されるまでその peer は `no-role` で断られる（既定拒否のまま）
- **狭める変更**: 記録（PR）は必須。merge を planner の L0 にしてよいかは §10.2 の 5
- **peer は自分の行（自分の役・自分が属する役の op）を広げる PR を通せない。operator も同じ**

これをどう強制するか:

- ゲートウェイは**表を置いた repo への書き込みを、表の中身に関係なく全部断る**（コードに固定。表で許せないようにする。表が自分を広げる経路を持たないため）。段 2 の提案 PR だけは、ゲートウェイ自身が自分の鍵で出す
- これで、S3（peer から書き込みの資格情報を外す）の後は、表の PR を merge できるのは kishibashi3 本人の `gh` だけになる。**operator の session も S3 で外す対象に含める**（operator が `kishibashi3` の `gh` を持ち続けると、operator は自分の行を広げられる）。§5.3 の非常口を operator に持たせるかは、ここと合わせて L1 で決める
- **S3 の前は強制できない。** peer も operator も `kishibashi3` の `gh` を持っているので、表の repo に直接 push / merge できる。それまでは監査（表の repo の merge のうち、kishibashi3 本人の操作でないもの）で見つけるだけ。名義が 1 つの構成では GitHub の author からは見分けられないので、footer と session 記録で突き合わせる（§7.2）
- **反映: ゲートウェイが表を自分で読む。operator はファイルを置かない。** ゲートウェイは表の repo の保護 branch（main）の head を、自分の鍵（段 1 は表の repo の owner の鍵。段 2 で読み取り専用の鍵ができたらそちら）で GitHub から取り、その commit sha を監査の `policy_sha`（§3.5）に書く。読み直しは `@scheduler` からの定期実行（と起動時）。手元のファイルを読む経路は作らない。表の repo と branch はゲートウェイの起動設定に書き、その設定もゲートウェイの uid だけが書ける場所に置く。ゲートウェイ自身は表を書き換えない。起動時の検証（§3.2）に通らない表は読み込まず、前の表のまま動いて operator に知らせる
  - これで「merge されていない表を読ませる」経路は、GitHub の main に入れる経路（＝上の merge の規則）か、ゲートウェイの起動設定・コードを書き換える経路のどちらかになる。後者は **#409 で uid を分けるまでは塞がらない**（同じ uid なら設定もコードも書き換えられる）。S3 までは §5.1 の「迂回」に入れた

---

## 4. 設計してほしいこと 3: inbound（`@gh-watch`）との関係

> **段 2。** inbound は通知で、①〜③（書き込みを止める）には関わらない。段 1 では作らない。下は段 2 で作るときの方針として残す

**別プロセスにする。部品（`packages/github-client`）は共有する。**

| | inbound（`@gh-watch`） | outbound（`@github`） |
|---|---|---|
| 要るもの | 公開 ingress + webhook secret | 鍵（App の秘密鍵 または PAT） |
| 置き場所 | ingress に近い所（hub は家庭内 Pi5） | 鍵を peer と別 uid で持てる所 |
| 落ちたとき | 通知が来ないだけ（書き込みは止まらない） | 書き込みが全部止まる |
| 攻撃面 | インターネットから届く | hub からだけ |

- 1 プロセスにすると、**インターネットに面したプロセスが鍵を持つ**ことになり、通知側の不具合で書き込みも止まる。分ける理由はこの 2 つ
- 将来の連携の例: `@github` が merge した後、`@gh-watch` に来る `pull_request.closed` を見て、依頼主に完了を知らせる。今は `@github` が API の応答で直接返すので必須ではない
- 実装言語は今は決めない。`iat.go` を使うなら Go、`gh-watch` / `scheduler` と揃えるなら Python。どちらでも設計は変わらない

### 4.1 inbound は App 必須か → 必須ではない

**repo の webhook は App なしで張れる。** GitHub の docs: 「You must be a repository owner or have admin access in the repository to create webhooks in that repository」（[Creating webhooks](https://docs.github.com/en/webhooks/using-webhooks/creating-webhooks)）。UI か `POST /repos/{owner}/{repo}/hooks` で作る。

**`@gh-watch` のコードは App に依存していない**（`packages/gh-watch/gh_watch.py` を読んだ範囲）:

- 検証に使うのは `X-Hub-Signature-256`（HMAC-SHA256）と `X-GitHub-Event` だけ。docs ではこの 2 つの header は webhook の配送全般に付く（[Webhook events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads) の Delivery headers。`X-Hub-Signature-256` は secret を設定した場合）
- payload の `installation` は読んでいない。docs では `installation` は「the event is configured for and sent to a GitHub App」のときだけ入るので、repo webhook で無くても困らない
- App の ID・秘密鍵は使っていない。README の「GitHub App 設定」節（App ID 4008981）が App 前提で書かれているだけ

App なしの inbound で変わるもの:

| | App の webhook | repo の webhook |
|---|---|---|
| 張る単位 | App 1 つに 1 か所。入れた repo 全部の event が来る | **repo ごとに 1 つずつ**張る（user 所有の repo には org webhook が無いので束ねられない）。repo を足すたびに張る作業が要る |
| 張るのに要る権限 | App の設定権限 | **その repo の admin**（fine-grained PAT なら `Webhooks: write`）。張る作業は人の操作にする（ゲートウェイの op には入れない。入れると webhook の向き先を変えられる = 外への送信経路になる） |
| secret | App に 1 つ | repo ごとに設定する（同じ値を使い回せる） |
| 公開 ingress | 要る | **要る**（違いは無い） |

- **どちらでも公開 ingress は要る。** 顧客環境で ingress が作れないなら、webhook の種類に関係なく inbound は落ちる。そのときの代わりは polling（Events API などを定期的に読む）だが、今の `@gh-watch` には無い（未実装。§7.2 の落ちるものに入れた）
- repo webhook で `@gh-watch` が使う event を受けられるか（[Webhook events and payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads)、2026-11-24 に読んだ）: `check_suite` は Availability に `repository` があり、「Repository and organization webhooks only receive payloads for the completed event types in repositories」。`@gh-watch` は `action == "completed"` だけを使うので足りる。`workflow_run` も Availability に `repository` があるが、その節の本文は逐語では読めていない（未確認として残す）

---

## 5. 設計してほしいこと 4: 直列にして何が守られ、何が守られないか（残るもの）

### 5.1 残るもの（「絶対に事故が起きない」とはならない）

| 残るもの | 例 | 何で減らすか | 消えない理由 |
|---|---|---|---|
| **ゲートウェイ自体の設定誤り** | `policy.yaml` で `@writer` に `pr.merge` を与えてしまう。鍵（App・PAT）に要らない権限を付けてしまう | 表の変更を review に通す。起動時の検証。既定拒否。拒否・許可の件数を毎日見る | 表が「正しく誤っている」場合は検証では分からない |
| **相手と鍵の対応の誤り** | **§1.3 の名乗り替え**。handle の付け替え（bridge を別 role に流用）で、古い役の割り当て（`members` の行）が新しい中身の peer に当たる | hub 側で handle ごとに資格情報を分ける（§8）。handle の付け替え時に表を見直す運用 | hub が handle を保証するまでは、ゲートウェイには名乗りの真偽が分からない |
| **迂回** | peer が `gh` / `curl` / `git push` で直接書く。IAT モードの `GH_TOKEN`。読み取りトークン以外のトークンが env やファイルに残っている。**ゲートウェイの監査 DB（§3.5）を書き換える**（§3.8 の提案と §9 の測り方はこの DB を根拠にしている）。**ゲートウェイの起動設定・コードを書き換えて、merge されていない表を読ませる**（§3.8 の反映） | S3 で peer から書き込みの資格情報を外す（§6）。監査 DB・起動設定・コードもゲートウェイの uid だけが書ける場所に置く（S3 の ④） | peer とゲートウェイが同じ uid の間は、ゲートウェイの鍵ファイルも監査 DB も起動設定も hub の PAT も peer から読み書きできる。**uid を分けるまでは塞がらない** |

その他に残るもの:

- **許された操作の中身の誤り**: 許可された `issue.comment` で誤った内容を書く、正しい権限で間違った PR を merge する。ゲートウェイは中身を判断しない（非エージェントなので）
- **ゲートウェイの乗っ取り**: 鍵が 1 か所に集まるので、そこが破られると全部の鍵の権限が出る。今は鍵が全 peer から読めるので、集めることで悪くはならないが、ゼロにもならない

### 5.2 直列で守られるもの・守られないもの

守られる（ゲートウェイを通る書き込み同士の間だけ）:

- 同じ PR・issue への書き込みが、ゲートウェイに届いた順に 1 つずつ実行される（対象ごとのキュー。repo 全体の 1 本のキューにはしない）
- **merge は sha 付き**にするので、「見た head と違う head を merge する」ことは、ゲートウェイの外で誰かが push した場合でも GitHub が断る（409）。これは直列とは別の守り方で、外の経路にも効く唯一の部分
- **approve は GitHub が断らない。** `commit_id` に古い sha を渡しても review はその commit に対して作られるだけで、断られない（[Create a review](https://docs.github.com/en/rest/pulls/reviews#create-a-review-for-a-pull-request) の応答は 200 / 403 / 422 だけ）。古い commit への approve が新しい push の後も required review に数えられるかは、branch protection / ruleset の「Dismiss stale pull request approvals when new commits are pushed」で決まる。**この設定を S0 の前提に入れる**（formal な approve を使う構成 A / C′ のとき。§7.2）。approve を使わず `LGTM ✅` コメントで代用する構成（B / C）では GitHub は何も見ないので、今の運用どおり LGTM の本文に head sha を書き、merge する側が merge の sha と突き合わせる

守られない:

- **ゲートウェイを通らない経路との競合**: kishibashi3 本人の操作（`k-ishibashi-ntv` での操作を含む）、GitHub Actions / CI、S3 までの間に `gh` を直接使う peer。これらとゲートウェイの書き込みの順序は保証できない（sha 付き merge を除く）
- **判断の競合**: 2 つの peer が同じ issue を同時に直し始める、といった作業の重複。GitHub API の順序の問題ではないので、直列にしても防げない
- **対象をまたぐ順序**: PR A の merge と PR B の作成の順序はキューが別なので保証しない
- **ゲートウェイに届く前の順序**: hub に先に届いた DM が先に処理される保証は、hub の配送順に依る

### 5.3 詰まり所としてのゲートウェイ

待ち:

- 1 件ごとに GitHub API 1〜数回（トークンが無ければ access_tokens も 1 回）。1 件あたり概ね 1 秒前後〜数秒の見込み（未計測、S1 で測る）
- 対象ごとのキューなので、別の PR への書き込みは並ぶが待たない。同じ PR に複数 peer が集まったときだけ待つ
- GitHub の rate limit は、App の構成なら installation 単位、PAT の構成なら PAT の持ち主のアカウント単位（§7.2）。いまは `kishibashi3` のユーザー単位の上限を全 peer で分けている。content を作る操作には secondary rate limit がある（数値は実装前に GitHub の docs で確かめる）。上限に当たったら、ゲートウェイは待たずに `failed (rate limited, retry after …)` を返す（peer が勝手に再送して詰まりを悪化させないよう、retry-after を伝える）

落ちたときに止まるもの:

| 止まるもの | いつから |
|---|---|
| peer からの `kishibashi3/*` と `ntv-contech/*` への書き込み全部（コメント・issue・PR・review・merge・push(P1)） | すぐ（S3 の後。S2 までは peer が `gh` を持っているので止まらない） |
| 読み取り（段 2 で R1 にした場合） | 渡したトークンの期限が切れたとき（App の構成で最長 1 時間後） |
| `@gh-watch` の通知 | 止まらない（別プロセス） |

落ちている間の依頼は hub の inbox に残る。再起動時の扱い:

- 依頼に有効期限を付ける（例: 受信から 30 分。値は S1 で決める）。過ぎた依頼は実行せず `expired` で返す（古い merge 依頼を後から実行しないため。sha 付きなので head が変わっていれば GitHub も断るが、それに頼らない）
- `msg_id` が監査にあれば実行しない（二重実行防止）
- 落ちたことを知る手段: `/ping`、`@scheduler` からの定期 `/ping`、operator の監視

非常口: ゲートウェイが落ちている間、書き込みは **kishibashi3 本人だけ**が自分の `gh` で行う。operator に非常口を持たせるかは L1 判断（持たせると operator の経路が迂回になる）。

---

## 6. 入れる段

#407 の制約（`agent-hub-roles-kaz/CLAUDE.md` §「破壊的更新は段階的 deprecation で（いきなり壊さない）」）に従い、いきなり `gh` を取り上げない。各段は **1 体で実機確認 → 残り** の順（staged rollout）。

**範囲は `kishibashi3/*` と `ntv-contech/*` の両方**（2026-11-26 に旧 S4「`ntv-*` を触る bridge は範囲外」を取り消した）。S1〜S3 は両方の owner の peer に同じ順で当てる。owner が増えたら、その owner の peer も同じ段を踏む。

| 段 | やること | peer の `gh` | 抜ける条件 | 戻し方 |
|---|---|---|---|---|
| **S0** | 本 doc。鍵の構成は段 1 では C0（いまある token 2 本、owner ごと。§7.1）に決まった。表（§3.2）の中身を決める: NTV の peer の一覧、`ntv-contech/frontend` の op の上限（§10.2 の 3） | 今のまま | 表の初版が専用 repo に merge される（kishibashi3 本人。§3.8） | — |
| **S1 観測** | ゲートウェイを起動し、`/ping` と `issue.comment` だけを、各 owner の 1 体ずつ（例: `@bridges-impl` と NTV の peer 1 体）から使う。①〜③の拒否（`owner-not-allowed` / `op-not-in-repo`）がわざと送った依頼で返ることも確かめる。同時に、peer が `gh` で何をしているかを session 記録から集計する（op 表の根拠） | 今のまま | op 表が集計で裏付けられる。待ち時間・失敗率が測れている | ゲートウェイを止める（peer は `gh` を持ったまま） |
| **S2 併用** | peer の書き込みを順にゲートウェイに切り替える（persona doc / bridge の指示を変える）。`gh` はまだ持っている。`gh` で書き込もうとしたら PreToolUse フックで**警告して**ゲートウェイを案内する（拒否はまだしない）。planner の LGTM 判定をゲートウェイの footer に切り替える（§3.1） | 持っている（**ゲートウェイは助言**） | 迂回の件数（§9 M2）が 1 週間 0 | 指示とフックを戻す（peer ごと） |
| **S3 外す** | peer から書き込みの資格情報を外す: ①`GH_TOKEN`（IAT モード）を渡さない ②`hosts.yml`（token 2 本とも）を peer の uid から読めなくする ③peer の `gh` は読み取りだけで動かす（R1 の読み取り鍵は段 2。それまでは R2。§3.6・§10.2 の 12） ④ゲートウェイの鍵ファイル・監査 DB・起動設定・コードを peer から読み書きできなくする。**2 つに分ける**: S3a は peer ごとの flag（例: bridge の `--gateway-only`）で ①〜③ を切り替え、元の資格情報は消さずに残す。S3b は S3a で問題が出ないことをログで確かめてから元の資格情報を消す | **書けない** | 迂回の件数が 0 のまま、peer の作業が回る | S3a: その peer の flag を戻して respawn（元の資格情報は残っている）。S3b の後は戻せない（資格情報の作り直しになる）ので、S3b は S3a を全 peer で 1 日以上回してから |

S3 の前提:

- **②④は uid を分けないと実現できない。** 今は全 bridge・operator・kishibashi3 が同じ uid（`kishibashi3`）で動いている。`hosts.yml` もゲートウェイの鍵も、mode 600 でも同じ uid なら読める（#73 の 2026-11-14 コメント「残るリスク」と同じ）
- **uid を分けるだけでは足りない。** 2026-11-24 の観測: uid `kishibashi3` は `docker` グループに入っている。docker を使える uid は bind mount で host のどのファイルでも読めるので、鍵を別の uid に置いても peer の uid から読める。また `~/.ssh/id_ed25519_pi5` が読めて Pi5 の `admin`（`docker` / `sudo` グループ）に入れるので、鍵を Pi5 に置いても辿れる。**peer の uid は `docker` / `sudo` グループに入れず、`~/.ssh` の鍵を読めないこと**が要る
- **uid を分ける作業には issue も担当も無かった。** 以前この doc では「publications#119 の結論待ち」と書いたが、publications#119 は 2026-10-30 に merge 済みの PR（Agent Authority Control Plane 構想の公開）で、待つ結論は存在しない（構想の参照としては §8 に残す）。待ち先として **#409** を立てた（2026-11-24）。**#409 の担当はまだ決まっていない。S3 は #409 が終わるまで始められない**
- uid を分けるまでの間（S2）にできるのは、フック（警告→拒否）・指示・監査による検出まで。**フックは `curl` や `git` 直叩きで迂回できるので、ガードレールであって境界ではない**
- §1.3 の名乗り替えも、hub の PAT が peer の env にある限り残る。S3 で ②④ と同時に、hub 接続の資格情報も peer ごとに分ける必要がある（#391 / #409）

S3 で外した後に peer が困る操作（例: `gh run rerun`、`gh project`、表に無い op）は、拒否の監査から拾い、表に足すかどうかを L1 で決める。

### 6.1 段 1 と段 2（2026-11-26 の見直し）

kishibashi3 の要件「あまり複雑にはしたくない」に合わせ、①〜③（§0）に要るものだけを段 1 に入れ、残りを段 2 に回した。S0〜S3 はどちらの段にも共通の入れ方の順番で、段 1 は S3 まで進める。

**段 1 に入れるもの:** ゲートウェイ本体（§3.1 の受け付け・§3.4 の断り方・§3.5 の監査）、owner と repo を入れた表（§3.2）、owner ごとの鍵 2 本（§7.1 の C0）、push の P1（§3.7）、表の記録の規則（専用 repo・PR・広げる変更は kishibashi3 本人。§3.8 の後半）。

**段 2 に回すもの:**

| 回すもの | 回した理由 | 回しても①〜③は成り立つか |
|---|---|---|
| 役ごとの鍵（§7.3） | いまある token は OAuth で fine-grained ではない。役ごとに分けても repo・op を絞れないので、鍵を増やすだけで上限は変わらない。分けて意味が出るのは fine-grained PAT か App にしたとき | 成り立つ。①②は owner ごとの鍵と表で、③は表で止める。役ごとの鍵は、表に誤りがあったときの上限を細かくするだけ |
| 表のメンテの機械提案（§3.8 の `/propose`） | 段 1 の表は役 3・owner 2・狭める repo 1 で、人が読める大きさ | 成り立つ。判定は既定拒否のまま。失うのは「使われていない許可」を機械が見つけることだけ |
| inbound（§4） | 通知で、書き込みを止める話ではない | 関わらない |
| 読み取り専用の鍵（§3.6 R1） | 段 1 の鍵（OAuth の `repo` scope）では読み取りだけの鍵が作れない。作るには新しく fine-grained PAT か App が要る | 成り立つ（①〜③は書き込みの目標）。**ただし S3 では書き込みと一緒に読み取りの資格情報も外れる**ので、S3 に入る前に R1 の鍵を段 2 から前倒しするか、読み取りもゲートウェイに通す（R2）かを決める（§10.2 の 12） |

**段 1 で①〜③が止まる時期:**

| | S1・S2 | S3 の後（#409 の後） |
|---|---|---|
| ① NTV 以外 → `ntv-contech` | 助言。peer は `hosts.yml` の `k-ishibashi-ntv` の token を自分で使える | 止まる（表 + 鍵の上限）。名乗り替え（§1.3）は残る |
| ② NTV → `kishibashi3/*` | 助言。peer は `kishibashi3` の token を自分で使える | 止まる（表 + 鍵の上限）。名乗り替えは残る |
| ③ `ntv-contech/frontend` の表に無い操作 | 助言 | 止まる（表だけ。鍵は frontend の admin 権限を持つので、ゲートウェイの不具合や表の誤りには上限が無い） |

---

## 7. 鍵の供給元（App あり・App なしのどちらでも動く）

kishibashi3 の要件（2026-11-23）: 「appって、お客さん環境で使えるかわからんから、app使うことも使わないこともできるようにしておいて」。

1 便目ではここを「#73 の答えで A / B / C のどれかを残す」分岐にしていた。**どれでも動く構成に書き直す。** App あり・App なしは**どちらも本線**で、どちらかが他方の縮退ではない。違いは §7.2 の「落ちるもの」で示す。

### 7.1 構成

ゲートウェイの中で鍵の種類を知っているのは**鍵の供給元**だけ。受け付け・判定・断り方・実行・記録（§3.1〜§3.8）は供給元に「この役の、この repo（owner）への、この op の鍵をくれ」と頼むだけで、返ってくるのが installation token か PAT かを見ない。

**段 1 は C0 で始める**（kishibashi3 の要件、2026-11-26）。A / B / C / C′ は段 2 以降の選択肢として残す。どの構成でも鍵は owner ごとに分かれる（fine-grained PAT は owner 1 つにしか効かず、App は owner ごとに installation がある。§7.3）。

| 構成 | 供給元 | 役 → 鍵 | 名義の数 |
|---|---|---|---|
| **C0. いまある token（段 1）** | `pat` | 役に関係なく **owner ごとに 1 本**: `kishibashi3` → `kishibashi3` の token、`ntv-contech` → `k-ishibashi-ntv` の token。どちらも `gh auth login` の OAuth token（scope `repo` を含む。`kishibashi3` 側は `delete_repo` なども） | owner ごとに 1（`kishibashi3` / `k-ishibashi-ntv`）。どちらも人のアカウント |
| **A. App 2 つ** | `app` | `review` → Reviewer App、他の役 → bot App | 2（`AgentHub [bot]` / `AgentHub Reviewer [bot]`） |
| **B. App 1 つ** | `app` | 全役 → bot App | 1 |
| **C. PAT** | `pat` | 役ごとに 1 本の fine-grained PAT（§7.3）。全部同じアカウントで作る | 1（PAT を作ったアカウント） |
| **C′. PAT + machine user** | `pat` | 役ごとの PAT を、名義を分けたいアカウント（machine user）で作る | アカウントの数（§7.4） |

設定の例（形の説明）:

```yaml
keys:
  provider: pat                 # 構成 C0（段 1）
  pats:
    kishibashi3: { path: /…/token-kishibashi3,     account: kishibashi3,     kind: oauth }
    ntv-contech: { path: /…/token-k-ishibashi-ntv, account: k-ishibashi-ntv, kind: oauth }
  owner_key: { kishibashi3: kishibashi3, ntv-contech: ntv-contech }   # 表の owners と同じ集合でないと起動しない
```

- C0 の鍵は `hosts.yml` から**写して**ゲートウェイの鍵ファイルに置く（ゲートウェイは `gh` の共有設定を読まない。§1.1 の `gh auth switch` に巻き込まれないため）。写した値は `hosts.yml` の値と同じなので、S3 で peer から `hosts.yml` を外すまでは peer も同じ鍵を持っている（§6.1）
- OAuth token には期限が無い（`expires_at` は書かない）。入れ替えは `gh auth refresh` か作り直しで、人の作業
- 起動時の検証で `permissions` との照合はしない（OAuth の scope は repo 単位の権限を表さない）。代わりに、鍵ごとに対象 owner の repo を 1 つ読めることを確かめる

```yaml
keys:
  provider: app                 # 構成 A / B
  apps:
    bot:      { app_id: ..., installation_id: ..., key_path: /…/agenthub-bot.pem }
    reviewer: { app_id: ..., installation_id: ..., key_path: /…/agenthub-reviewer.pem }   # 構成 A のときだけ
  role_key: { review: reviewer, impl: bot, plan: bot, read: bot }   # read は §3.6 R1 の読み取り鍵。owner ごとに installation_id が要る
```

```yaml
keys:
  provider: pat                 # 構成 C / C′
  pats:
    review: { path: /…/pat-review, owner: kishibashi3, permissions: { pull_requests: write, issues: write }, expires_at: 2027-11-24T00:00:00Z }
    impl:   { path: /…/pat-impl,   owner: kishibashi3, permissions: { contents: write, pull_requests: write, issues: write }, expires_at: ... }
    plan:   { path: /…/pat-plan,   owner: kishibashi3, permissions: { contents: write, pull_requests: write, issues: write }, expires_at: ... }
    read:   { path: /…/pat-read,   owner: kishibashi3, permissions: { contents: read, pull_requests: read, issues: read, checks: read, actions: read }, expires_at: ... }
  role_key: { review: review, impl: impl, plan: plan, read: read }   # 権限の組が同じでも impl と plan は共有しない（§7.3）
```

- 構成は起動時に 1 つに決まる。動いている間に切り替えない（落ちたら別の構成で試す、もしない。§3.4）
- 起動時の検証: 役の op に要る権限が、その役の鍵の権限に入っているか。App は App の permissions と照らす。**PAT は GitHub から PAT の権限を引く手段を確かめていないので、設定に書いた `permissions` と照らす**（書いた値が実物と違えば、実行時に GitHub が 403 で断り、送り主には `key-unavailable` で返る）
- PAT の `expires_at` の 14 日前から operator に毎日知らせる。期限を過ぎた鍵の役は `key-unavailable` で断る

外から見て変わらないもの: DM のコマンド（§3.1）、断る理由の語彙（§3.4）、監査の列（§3.5）、footer（`@<from> via @github`）、メンテの提案（§3.8）。

### 7.2 落ちるもの（App あり / App なし）

段 1 の C0 は、下の表の **C の行と同じで、さらに「classic しか使えない環境」（§7.3）の扱い**になる: 名義は人のアカウント、formal な approve は使えない（`LGTM ✅` で代用）、依頼ごとにも役ごとにも絞れず、上限は「その owner でアカウントが触れる repo 全部・`repo` scope 全部」。読み取り専用の鍵が無いので R1 は成り立たない（§3.6）。

| | App あり（A / B） | App なし（C / C′） |
|---|---|---|
| **GitHub 上の名義** | A: 2 つ / B: 1 つ。どちらも bot で、人（kishibashi3）と AI の区別は付く | C: PAT の持ち主 1 つ（**人と AI の区別も付かない**）/ C′: machine user の数だけ（§7.4） |
| **formal な approve** | A: 使える / B: 使えない | C: 使えない / C′: 作者と別のアカウントの役なら使える。**使えない構成では `LGTM ✅` コメントでの代用が残る**（§7.4） |
| **依頼ごとの絞り込み** | 依頼 1 件分（その repo・その op の権限）まで、1 時間のトークンで絞れる | **絞れない。** 役の op 全部・役の repo 全部が上限（§7.3） |
| **鍵の期限と入れ替え** | 秘密鍵に期限は無い。トークンは 1 時間で自動 | PAT ごとに期限（最長 366 日 か 無期限。org の方針で短くされうる）。**入れ替えは人が UI で作り直す作業**で、鍵の数（= 役の数）だけある |
| **読み取りトークン（§3.6 R1）** | 1 時間のトークンを都度作って渡す | 事前に作った読み取り PAT を渡す。**漏れたら PAT の期限まで読める。** classic PAT しか使えない環境では R1 が成り立たず R2 |
| **push（§3.7）** | P1 / P2 とも可 | P1 だけ（P2 は窓が PAT の期限まで開く） |
| **rate limit** | installation 単位 | PAT の持ち主のアカウント単位。C では人（kishibashi3）の手作業と同じ枠を分け合う |
| **迂回の見分け（§9 M1/M2）** | 定義はどちらも同じ（**監査の `result_url` に無い書き込み = 迂回**、§9）。App ありでは GitHub の author が bot か kishibashi3 かでも裏が取れる | C では author が全部同じなので、監査の `result_url` との突き合わせだけになる |
| **表の repo の強制（§3.8）を GitHub 側で見る** | 提案 PR の author が bot、merge が kishibashi3 で、誰の操作かが GitHub に残る | C では全部 kishibashi3 名義。ゲートウェイの監査と footer で見る |
| **inbound（§4.1）** | App の webhook 1 か所 | repo ごとの webhook。repo を足すたびに人が張る。**公開 ingress が要るのは同じ** |
| **導入の前提（顧客環境）** | App を作って入れられること（org の方針で App を入れられないことがある。どういう制限がありうるかは未確認） | PAT が許されていること。org は「fine-grained PAT に owner の承認を要る」「PAT で org の資源に触れない」「classic PAT を禁止する」を設定できる（[PAT policy](https://docs.github.com/en/organizations/managing-programmatic-access-to-your-organization/setting-a-personal-access-token-policy-for-your-organization)） |

**どちらも使えない環境**（App の install も PAT も許されない）ではゲートウェイは動かない。そのときは `key-unavailable` で全部断り、起動時に operator に知らせる。

**ingress が作れない環境**では、App の有無に関係なく inbound が落ちる（polling は未実装）。

### 7.3 PAT は依頼ごとに絞れない → 役ごとに鍵を作る

確かめた結果（GitHub docs）:

- PAT は人が事前に作る。docs の [PAT のページ](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens) にある作り方は UI の手順（と UI の URL に値を詰めておく方法）だけで、**API で作る手段は見つけられなかった**
- 呼び出しのときに権限や repo を絞る引数は PAT には無い。PAT で使える権限は「作ったときに付けた permissions / scope」で決まる。installation token の `repositories` / `permissions`（[access_tokens](https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app)）に当たるものは無い
- fine-grained PAT は **resource owner（user か org）1 つ**にしか効かない（「Each token is limited to access resources owned by a single user or organization」）
- classic PAT は scope 単位で、repo を絞れない（`repo` scope は持ち主が触れる repo 全部の読み書き）

→ **「鍵を複数用意して選ぶ」形になる。** 鍵は少なくとも resource owner ごとに 1 本要る。

**段 1（C0）: owner ごとに 1 本。** いまある 2 本は OAuth token（classic と同じく scope 単位）で、repo も op も絞れない。役ごとに鍵を分けても上限は変わらないので、分けない。**op の制限はゲートウェイの表だけが持つ。** 鍵が上限として効くのは owner をまたぐ書き込みだけで、2026-11-26 に確かめた範囲では:

- `kishibashi3` の token からは `ntv-contech` の repo が 1 つも見えない（`gh repo list ntv-contech` が 0 件、`gh repo view ntv-contech/frontend` が「Could not resolve to a Repository」）→ 表が誤って NTV 以外の peer に `ntv-contech` を許しても、`kishibashi3` の鍵では書けない。ただしゲートウェイは owner で鍵を選ぶので、`ntv-contech` 宛ての依頼には `k-ishibashi-ntv` の鍵が使われる。**①の鍵の上限が効くのは「鍵の選び方の不具合」に対してで、表の `members` の誤りに対しては効かない**
- `k-ishibashi-ntv` の token は、見える `kishibashi3/*` の repo 28 個すべてで `viewerPermission` が `READ` → ②は鍵の上限でも止まる（コラボレーターを足せば変わる。足すのは広げる変更と同じく kishibashi3 本人の判断）
- `k-ishibashi-ntv` は `ntv-contech/frontend` で `ADMIN` → ③に鍵の上限は無い。表だけが止める

**段 2 以降: 役 × owner ごとに 1 本**（fine-grained PAT か App にしたとき）。

- 鍵の数 = 役の種類 × owner の数。`plan` と `impl` の権限の組が同じでも、監査の `key_id` で役を区別できなくなるので共有しない
- peer の数には比例しない（§3.2 で役に束ねた効果）
- classic / OAuth しか使えない環境では、段 1 と同じく owner ごとに 1 本のままにする

### 7.4 App なしのときの名義と approve

- **C（同じアカウントの PAT）では名義は 1 つ。** peer が出した PR も、approve しようとする `@reviewer` も同じアカウントなので、approve は作者と同じ名義になり使えない（今と同じ。§1.1）。**`LGTM ✅` コメントでの代用が残る**。ゲートウェイは `pr.review approve` を `same-identity` で断り、送り主には `pr.comment` で `LGTM ✅` を書くよう返す（どの構成でも同じ語彙）
- **C′（machine user）なら App なしでも名義を分けられる。** GitHub の規約は machine account を認めている（「We do permit machine accounts」「A machine account is used exclusively for performing automated tasks」）。ただし**無料の machine account は、無料の個人アカウント 1 つにつき 1 つまで**（「You may maintain no more than one free machine account in addition to your free Personal Account」、[ToS B.3](https://docs.github.com/en/site-policy/github-terms/github-terms-of-service)）
  - 無料の範囲では、名義は「人のアカウント + machine account 1 つ」の 2 つまで。例: `impl` / `plan` の役を machine account で、`review` の役を人のアカウントの PAT で動かせば、formal な approve はできる。**ただしその approve は人（kishibashi3）の名義になり、人と AI の区別が GitHub 上で付かなくなる**
  - 3 つ以上に分けるには有料のアカウント（org の seat）が要る。顧客環境で machine user を作れるかは顧客の方針次第
- 名義を分けた approve が branch protection の required review に数えられるか（approve するアカウントに要る権限）は未確認。App の構成でも同じ問いで、#73 の完了条件 1 で確かめる項目

### 7.5 App を使う構成で #73 が決めること

#73 の答えで変わるのは、**App を使う構成（A / B）のときの中身だけ**。App を使わない構成（C / C′）は #73 を待たずに動かせる。

| #73 の答え | App の構成でゲートウェイができること |
|---|---|
| App の数 | 2 つなら構成 A、1 つなら構成 B |
| Contents: write あり | §3.7 の push を App 名義で行える。`pr.merge` も可 |
| Contents: write なし | push と merge は App ではできない。**その op だけ PAT の鍵を使う、という混ぜ方はしない**（構成は 1 つ。§7.1）。push と merge を役に許さず、人の経路に残す。S3 で外せない経路が残る |
| Workflows: write なし | `.github/workflows/` を含む push は App では断られる。その push だけ人の経路に残す |
| Issues: write を Reviewer App に付けない（planner 案） | 構成 A で `review` の役に `issue.create` を許さない（表で許しても鍵が断る） |
| 入れる repo | 入れた repo だけが App の構成の対象。入っていない repo への依頼は `key-unavailable` で返す（表の `owners` と installation を合わせる） |

鍵の置き場: #73 の 2026-11-14 コメントの案（kishibashi3 が置き、operator も peer も中身を見ない）を、App の秘密鍵にも PAT にも同じく使う。違いは 1 点: **ゲートウェイの uid だけが読める**ように置く（S3 の ④）。uid を分けるまでは今の案と同じ。

---

## 8. 他の issue との関係

| issue | 関係 |
|---|---|
| **#73**（App の権限と入れ先） | **App の構成（A / B）の前提。** PAT の構成（C / C′）は #73 を待たない（§7.5）。#73 が「各 bridge に `GH_TOKEN=<IAT>` を渡す」形で先に進んだ場合、その形はゲートウェイの S3 で外す側になる（IAT を peer に渡すと 1 時間は App の全権限が peer にあるため）。#73 の完了条件「approve」「実測」は、ゲートウェイ経由でも満たせる |
| **#391**（fleet.env のトークンを用途ごとに分ける） | 並行。ゲートウェイが入っても hub 接続の `GITHUB_PAT` は全 peer に要る。§1.3 の名乗り替えを塞ぐには、この PAT を peer ごとに分ける（か hub 側で handle ごとの資格情報にする）必要があり、それは #391 / #409 の範囲 |
| **#398**（`gh` トークンの `delete_repo` / `admin:public_key` を外す） | 並行・先にやってよい。**段 1 ではこの token をゲートウェイの `kishibashi3` の鍵として使う**（§7.1 の C0）ので、外さなければゲートウェイの鍵が `delete_repo` を持つ（ゲートウェイは呼ばないが、乗っ取られたときの上限になる。§5.1）。ゲートウェイを待たずに外すのがよい |
| agent-hub#543（hub 接続 PAT の入れ替え） | 値の入れ替えだけ。本設計とは独立 |
| **#409**（peer と鍵を別の uid に分ける） | **S3 の前提。** 担当は未定。2026-11-24 に本設計から立てた（§6） |
| publications#119（Agent Authority Control Plane 構想、2026-10-30 merge 済みの PR） | 構想の参照。待ち先ではない |
| `@gh-watch`（#233、agent-hub#289） | §4。別プロセス。webhook は App のものでも repo のものでもよい（§4.1） |

---

## 9. 測り方

| | 何を | どう測る | 目標 |
|---|---|---|---|
| M1 | `kishibashi3/*` と `ntv-contech/*` の書き込みのうちゲートウェイ経由の割合（owner ごとに出す） | **GitHub 側の書き込み（コメント・PR・review・merge）を集め、監査の `result_url` と突き合わせる。** 監査にあればゲートウェイ経由。鍵の種類によらず同じ測り方（App の構成なら author が bot かどうかでも分かるが、それに頼らない） | S2 の終わりで、peer の書き込みはほぼ全部ゲートウェイ経由 |
| M2 | 迂回の件数 | M1 で集めた書き込みのうち、**監査の `result_url` に無いもの**（§7.2 と同じ定義。footer は peer が付けなければ数えられないので使わない）。ここから人（kishibashi3 本人）と CI の書き込みを除く。CI は author（`github-actions[bot]` 等）で除ける。人の書き込みは、名義が 1 つの構成では author で分けられないので、peer の session 記録（§11 と同じ材料）に当たる操作があるかで分ける。S2 では PreToolUse フックの警告件数も | S3 の後 0 |
| M3 | 拒否の件数 | 監査の `decision=denied` を送り主・理由ごとに | 0 にはならない。理由の内訳を見て、表の不足か peer の誤りかを分ける |
| M4 | 待ちと失敗 | 監査の `started_at - received_at`、`finished_at - started_at` の p50 / p95、`failed` / `unknown` の件数 | S1 で基準値をとる |
| M5 | 落ちていた時間 | `/ping` の無応答時間、落ちている間の `expired` 件数 | |
| M6 | peer の資格情報 | S3 の後、各 peer のプロセス env（キー名だけ）に `GH_TOKEN` が無い。peer の uid から `hosts.yml` とゲートウェイの鍵が読めない | 全 peer で無い・読めない |

---

## 10. 決めたこと・決めていないこと

### 10.1 決めたこと（2026-11-24。可逆なので本設計の中で決め、operator に報告した）

| | 決めたこと | 根拠 |
|---|---|---|
| 読み取り | **R1**（§3.6）。classic PAT しか使えない環境では R2。**2026-11-26: 段 2 に回した**（段 1 の C0 では R1 が成り立たない。S3 の前に何を使うかは §10.2 の 12） | 読み取りが `gh` 呼び出しの 9 割（§11） |
| push | **P1**（§3.7） | PAT の構成では P1 しか成り立たない。App の構成でも鍵を peer に渡さない |
| メンテの値 | **K = 3・窓 14 日、N = 60 日**（§3.8） | §11 の頻度。運用で直す |
| 置く host | **bridge host**。鍵はゲートウェイ専用の uid だけが読める | P1 の bundle をファイルで受け渡すので peer と同じ host が要る。Pi5 に置いても peer の uid から ssh で辿れる（§6）ので隔離は増えない。Pi5 はメモリが逼迫している。**条件（鍵を peer と別 uid で持てる）は、#409 で peer の uid を `docker` / `sudo` から外して初めて満たせる** |
| 鍵の混ぜ方 | 操作ごとに鍵を混ぜない（§7.1） | どの鍵で通ったかが追えなくなる |
| 範囲と段 1（**kishibashi3、2026-11-26**） | 範囲は `kishibashi3/*` と `ntv-contech/*`（owner は将来増える前提）。段 1 の目標は §0 の①〜③。鍵は C0（いまある token 2 本、owner ごと）。役ごとの鍵・機械提案・inbound・R1 は段 2（§6.1） | issuecomment-7189759185 |
| 表の repo | 表は専用 repo に置き、ゲートウェイがその main から自分で読む（operator はファイルを置かない）。表を置いた repo への書き込みは表の中身に関係なくコードで断る（§3.8） | 表が自分を広げる経路を持たない。merge されていない表を読ませない |

### 10.2 決めていないこと

| # | 何を | 誰が・いつ |
|---|---|---|
| 1 | **段 2 でどの構成に移るか**（§7.1 の A / B / C / C′）。段 1 は C0 に決まった（§10.1）。どれを選んでもゲートウェイの外の振る舞いは同じ | kishibashi3。段 2 に入るとき |
| 2 | #73: App を使う構成のときの App の数・権限・入れ先（§7.5）。`ntv-contech` に入れるかを含む | 1 に従属。A か B のときだけ |
| 3 | 役の種類と、役ごとにどの op を許すか（§3.2 の表の中身）。peer をどの役と owner に割り当てるか（**どの peer が NTV か**。2026-11-26 の `get_participants` では `@ntv-*` の 9 体と `@cae-coder` が `ntv-contech` の repo を触る名前に見えるが、確かめていない）。**`ntv-contech/frontend` で許す op**（③の中身） | §11 の集計を根拠に決める。既定は拒否。owner の割り当てと frontend の上限は広げる変更なので kishibashi3（S0 で表の初版として） |
| 5 | 狭める PR の merge を planner の L0 にしてよいか（§3.8。広げる PR は kishibashi3 本人だけ） | operator が planner に回す（締める方向なので kishibashi3 の GO は不要） |
| 8 | operator に非常口を持たせるか（§5.3。§3.8 の「operator も自分の行を広げられない」と合わせて決める） | **kishibashi3**（operator の権限を緩める話なので operator は決められない） |
| 9 | uid の分け方 | **#409**。担当未定 |
| 11 | C′ で machine user を作るか（無料の範囲では名義は 2 つまで。§7.4） | 1 に従属。C′ のときだけ |
| 12 | **S3 で peer の読み取りをどうするか**。書き込みの資格情報を外すと、C0 では読み取りも一緒に外れる（§3.6）。案: R1 の読み取り専用の鍵（fine-grained PAT か App、owner ごと）を S3 の前に段 2 から前倒しする / 読み取りもゲートウェイに通す（R2、詰まり所が大きい） | kishibashi3。S3 に入る前まで（S1・S2 は決めなくても進む） |

番号は旧 §10 のもの（4・6・7・10 は 10.1 に移した）。

---

## 11. S1 の集計: peer が `gh` で何をしているか

### 11.1 取り方

- 材料: この host の `~/.claude/projects/*/*.jsonl`（Claude Code の session 記録）の Bash の tool_use のうち、`gh ` を含むコマンド。残っていた範囲は **2026-09-14〜2026-11-23**（約 70 日）
- peer の範囲: workdir が `~/app/private/agent-hub*`（`ntv-*` の役を除く）と `/tmp/*`（実機確認用の workdir）の記録。`ntv-planner` / `ntv-reviewer` は集計した 2026-11-24 時点では範囲外（旧 S4）だったので別に数えた。2026-11-26 に範囲に入れたが、集計はやり直していない（`~/app/ntv/*` の workdir の peer も数えていない）
- 操作の分け方: `gh <group> <sub>`、`gh api` は method とパス（番号・sha を伏せる）
- 限界:
  - **workdir 単位で、peer 単位ではない。** 同じ workdir に人や operator の session が混ざる（例: bridges の workdir にはこの doc を書いている session も入る）
  - heredoc の本文に書いた `gh ...` の文字列も拾う。例: planner の `repo delete` 2 件は issue 本文の中の文字列だった。**件数は多めに出る**
  - `gh api -f` で `-X` が無いものは POST に数えた。`search/*` は実際は読み取りなので書き込みから除いた
  - `~/.codex/sessions` には `gh` の呼び出しが 0 件だった。codex の bridge が別の場所に記録しているかは確かめていない
  - 待ち時間・失敗率（§9 M4）はゲートウェイを動かしていないので測っていない。S1 のもう半分（ゲートウェイを 1 体から使う）は実装が要る

### 11.2 全体

| | 件数 | 割合 |
|---|---|---|
| peer の `gh` 呼び出し | 11,720 | |
| 読み取り | 10,531 | 90% |
| 書き込み | 650 | 6% |
| `gh auth`（`switch` / `token` / `status`） | 539 | 5% |

読み取りの上位: `pr view` 6,187 / `pr list` 1,535 / `pr checks` 880 / `issue view` 819 / `issue list` 174 / `pr diff` 101 / `project item-list` 89 / `api graphql` の query 41 / `api GET .../issues/{n}/timeline` 40。

**`gh auth token` / `auth switch` が peer から使われている。** peer がトークンの値そのものを取り出せる・アカウントを切り替えられる、ということで、S3 で外す対象（§6 ②）がそのまま使われている証拠になる。

### 11.3 書き込み（workdir ごと）

| workdir（役） | 書き込みの内訳 |
|---|---|
| planner | `project item-edit` 70、`issue comment` 29、`pr merge` 26、`issue edit` 17、`issue create` 10、`project item-add` 10、`api graphql` mutation 9、`label create` 8、`issue close` 7、`project field-create` 4、`project item-archive` 2、`pr comment` 2、`pr review` 2、`api PATCH` コメント編集 2、`pr edit` 1、`api DELETE refs/heads/...` 1、`project create` 1 |
| operator | `pr merge` 60、`issue create` 27（+`api POST issues` 3）、`issue comment` 25（+`api POST .../comments` 21）、`pr comment` 17、`issue close` 11、`variable set` 6、`issue edit` 3、`pr close` 2、`api POST .../sub_issues` 2、`api DELETE` コメント削除 2、`workflow run` 1、`pr create` / `pr edit` / `pr ready` 各 1 ほか |
| reviewer | `pr comment` 54、`api PATCH` コメント編集 1、`issue create` 1 |
| writer-ja | `pr create` 25、`pr edit` 14、`issue create` 2、`issue comment` 1、`pr ready` 1、`issue delete` 1 |
| 実装系（bridges / control / agent-hub / installer / plugins-claude） | `pr create` 33、`pr comment` 38、`issue create` 19、`issue comment` 23、`pr edit` 9、`issue close` 5、`pr close` 4、`issue edit` 2 |
| philosopher | `issue comment` 8、`pr create` 5、`issue create` 3、`issue close` 2、`pr edit` 2、`pr comment` 1 |
| model-scout | `issue comment` 1 |
| （集計時は範囲外）ntv-planner / ntv-reviewer | `pr comment` 39、`issue create` 6、`issue comment` 5、`issue edit` 4 ほか |

`git push` を含むコマンド（P1 の対象）: writer-ja 197、philosopher 61、reviewer 38、planner 34、bridges 27、control 23、operator 20 ほか（同じく heredoc の文字列を含む）。

### 11.4 表（§3.2）への含意（決定ではない。§10.2 の 3 の材料）

- **§3.1 の候補に無い op が使われている**: `pr edit`、`pr close`、`pr ready`、`issue edit`、コメントの編集・削除（`api PATCH` / `DELETE .../issues/comments/{n}`）、`project item-edit` / `item-add`、`label create`、`api graphql` の mutation、`.../sub_issues`、`variable set`、`workflow run`
- 役の形はおおむね 4 つに分かれる: **コメントだけ**（reviewer）、**issue / PR を作って直す**（writer-ja・実装系・philosopher）、**それに merge・project・label を足す**（planner）、**operator**（merge・variable・workflow を含む。§10.2 の 8 と一緒に決める）
- **表に入れないほうがよい候補**: `issue delete`（writer-ja 1 件）、`api DELETE refs/heads/...`（planner 1 件）、`project create` / `field-create`、`variable set`。どれも件数が少なく、取り消せないか設定を変える操作。既定の拒否に残し、要るときは断られた記録から足す（§3.8）
- `project` 系（planner 87 件）は GitHub Projects の操作で、repo の権限とは別の権限になる。App の構成でこれを App で扱えるか（user が持つ Projects を含む）は確かめていない。§7.5 の #73 の判断に入れる

## 調査済み事項

- issue #407 本文と 2026-11-23 の制約コメント 2 件（issuecomment-7116121242 権限表のメンテ / issuecomment-7116190995 App を使う・使わない）、#73（本文と 2026-06-09 / 2026-08-21 / 2026-10-30 ×2 / 2026-11-14 ×2 のコメント）、#391（本文と 2026-10-30 の調査コメント）、#398、agent-hub#543
- `packages/gh-watch/README.md` と `gh_watch.py`（webhook の HMAC 検証は `X-Hub-Signature-256`、event は `X-GitHub-Event`、payload の `installation` も App の資格情報も使っていない。購読の sender 単位管理、処理後に mark_as_read）
- `packages/github-client/iat.go`（access_tokens を body なしで呼んでいる = 権限と repo を絞っていない）
- `agent-hub/packages/scheduler/README.md` と `scheduler.py` の `_check_owner`（送り主 = owner の entry だけ操作可）
- `agent-hub/src/mcp/server.ts` の auth middleware（`X-Participant-Id` による handle override、`AGENT_HUB_STRICT_HANDLE_OWNERSHIP`）
- `bridge-claude2/cmd/bridge/runner.go` の IAT モードと `filteredEnv`（`GITHUB_APP_*` だけ除く）、`bridge-codex2/cmd/bridge/runner.go`（`GITHUB_PAT` を内側に渡す）
- GitHub docs（2026-11-24 に読んだ）: [PAT の管理](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens)（作成は UI の手順だけ、resource owner は 1 つ、期限は 1〜366 日か無期限）、[org の PAT 方針](https://docs.github.com/en/organizations/managing-programmatic-access-to-your-organization/setting-a-personal-access-token-policy-for-your-organization)（承認必須・PAT 禁止・classic 禁止を設定できる）、[installation token の作成](https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app)（`repositories` / `permissions` で絞れる、1 時間）、[webhook の作成](https://docs.github.com/en/webhooks/using-webhooks/creating-webhooks)（repo の admin なら App なしで作れる）、[webhook の event と payload](https://docs.github.com/en/webhooks/webhook-events-and-payloads)（配送 header は共通、`installation` は App 宛てだけ）、[ToS B.3](https://docs.github.com/en/site-policy/github-terms/github-terms-of-service)（machine account は無料の個人アカウント 1 つにつき 1 つまで）
- §11 の集計: `~/.claude/projects/*/*.jsonl` の Bash の tool_use を Python で数えた（2026-11-24）。`~/.codex/sessions` も見た
- uid と隔離（§6・§10.1）: `id`（uid 1000、groups に `sudo` `docker`）、`ls -l /var/run/docker.sock`（`root:docker` 660）、`getent passwd`、`ls -l ~/.ssh`、Pi5 に `ssh` して `id` と `docker ps` が通ること、`sudo -n true` がパスワードを要求すること
- `agent-hub-roles-kaz/CLAUDE.md` の「git 運用規約」（main 直接 push）と「破壊的更新は段階的 deprecation で」（flag を false にして運用 → 問題なければ削除。§6 の S3a / S3b）
- `packages/gh-watch/gh_watch.py` の `check_suite` / `workflow_run` の扱い（`action == "completed"` だけを使う）
- [Create a review](https://docs.github.com/en/rest/pulls/reviews#create-a-review-for-a-pull-request) の応答（200 / 403 / 422。reviewer の指摘で知り、§5.2 に反映）
- publications#119: `gh pr view` で 2026-10-30 に merge 済みの PR（題「docs(ai): Agent Authority Control Plane 構想を公開」）と確かめた
- 2026-11-26 の見直しで確かめたこと: issuecomment-7189759185（planner が kishibashi3 の要件を逐語で記録）。`gh auth status`（`kishibashi3`: `admin:public_key` `delete_repo` `gist` `project` `read:org` `repo` `workflow` / `k-ishibashi-ntv`: `gist` `read:org` `repo` `workflow`。token の値は表示していない）。`gh repo view --json viewerPermission` をアカウントごとに: `kishibashi3` → `kishibashi3/agent-hub-bridges` は `ADMIN`、`ntv-contech/frontend` は見えない（`gh repo list ntv-contech` も 0 件） / `k-ishibashi-ntv` → `ntv-contech/frontend` は `ADMIN`、`kishibashi3` の repo（一覧で見える 28 個）はすべて `READ`。`gh repo list ntv-contech`（20 repo、すべて private）。`get_participants`（`@ntv-*` 9 体と `@cae-coder` が online。NTV の peer かは確かめていない、§10.2 の 3）
- 未確認: `gh-watch` の実動作、peer ごとに絞った installation token の実挙動、名義を分けた approve が required review に数えられるか、GitHub の rate limit の現在値、codex の bridge の `gh` 操作（記録の場所）、**fine-grained PAT を API で作る手段・PAT の権限を API で引く手段が本当に無いか**（docs の該当ページに見当たらなかっただけ）、repo webhook で `workflow_run` を受けられるか（Availability に `repository` はあるが本文を逐語で読めていない。`check_suite` は確かめた、§4.1）、古い `commit_id` への approve の実挙動（docs の応答一覧からの判断。実機では試していない）、`gh` が `hosts.yml` の installation token / fine-grained PAT を使えるか（§3.6）、org が App の install をどう制限できるか、作者が自分の PR を approve できないことの docs 上の記述（今の運用で観測している事実として書いた）

