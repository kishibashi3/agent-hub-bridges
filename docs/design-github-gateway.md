# GitHub ゲートウェイ peer 設計（issue #407）

GitHub への書き込みを 1 つの非エージェント peer に通し、**DM の送り主ごとに使う鍵と許す操作を切り替える**。

- 状態: **設計のみ。実装はしていない。**
- 正本の依頼: kishibashi3/agent-hub-bridges#407
- **#73 の答え待ちで止めている所がある**（§7）。#73 の答えによって変わる所は「#73 がこう決まればこう」と分岐で書いた
- 時刻は RFC 3339 UTC で書く

---

## 0. 結論（先に）

1. **非エージェント peer `@github`（仮称）を新設する。** `@scheduler` と同じく、DM の `/` コマンドを決まった形でだけ受け、LLM を通さない。送り主の handle から、**ポリシー表に書かれた操作だけ**を、**その送り主用に権限を絞って作ったトークン**で実行する。それ以外は断る
2. **inbound（`@gh-watch`）とはプロセスを分ける。** 同じ GitHub App を使い、部品（`packages/github-client`）を共有する。落ち方と置き場所が違うため（§4）
3. **ゲートウェイを入れただけでは何も守れない。** 各 peer が `gh` と共有トークンを持ったままなら、ゲートウェイは助言にすぎない。**外す段（S3）と、その前提になる「peer と鍵を別の uid に分ける」ことを段の中に入れた**（§6）
4. **「絶対に事故が起きない」とはならない。** 残るもの: ゲートウェイの設定誤り／相手と鍵の対応の誤り／迂回。さらに、**今の hub では送り主の名乗りを偽れる**（§1.3）。これは「対応の誤り」と「迂回」の両方につながる。残るものは §5 にまとめた
5. **人・CI・ゲートウェイを通らない peer との競合は守れない**（§5.2）
6. **ゲートウェイが詰まり所になる。** 落ちると kishibashi3/* への書き込みが全部止まる（§5.3）

---

## 1. 現状の問題

### 1.1 書き込みの経路

観測: この host（bridge host）の作業ツリーとコード、#73・#391・#398 の operator / planner の観測。2026-11-24 時点。

- 全 bridge の内側プロセスが `gh` を使える。認証は `~/.config/gh/hosts.yml` の `kishibashi3` トークン。scope は `admin:public_key` `delete_repo` `gist` `read:org` `repo` `project`（#398）。**どの peer も、どの repo に対しても、merge・削除を含めて何でもできる**
- git の push も `gh` を credential helper にしている環境では同じトークンを通る（#73 の 2026-10-30 コメント、論点 C）
- 全 peer が同じアカウントなので、GitHub 上の author は全部 `kishibashi3`。人と AI の区別も、どの peer かの区別も GitHub 側に残らない。role は footer の文字でしか分からない
- 作者が自分の PR を approve できないため、formal な approve が使えず `LGTM ✅` コメントで代用している

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

これは本設計の中では塞げない。塞ぐには hub が handle ごとに別の資格情報を持つ必要がある（§8 の publications#119 / #391）。それまでの間、ゲートウェイの「相手ごとの切り替え」は**事故（peer が間違った操作を頼む）には効くが、意図的な・あるいは LLM が試行錯誤で行う名乗り替えには効かない**。§5.1 に残るものとして書く。

---

## 2. 案の全体

```
  peer (LLM)                 @github (非 LLM)                        GitHub
  ───────────               ──────────────────                      ──────
  DM: /pr comment ...  ──▶  1. from を読む
                            2. policy.yaml で (from, op, repo) を引く
                               無ければ 拒否 → DM で返す + 監査に記録
                            3. (from 用の App, repo, op に要る権限だけ)
                               で installation token を作る           ──▶ access_tokens
                            4. 対象ごとの直列キューに入れて実行        ──▶ REST API
                            5. 結果を監査に記録 → DM で返す
  ◀── 結果 DM (caused_by)
```

- 鍵（App の秘密鍵）はゲートウェイの uid だけが読める場所に置く。**peer には秘密鍵も書き込み用トークンも渡さない**
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

最初に入れる操作の候補（実際に peer が使っている操作の集計は S1 でとる。今は未集計）:

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

ゲートウェイは本文の末尾に footer を**自分で**付ける（`@<from> via @github`）。peer が書いた footer は信用しない。

### 3.2 ポリシー表

`policy.yaml`（例）。**書かれていないものは全部拒否**（既定拒否）。

```yaml
version: 1
apps:
  bot:      { app_id: ..., installation_id: ..., key_path: /…/agenthub-bot.pem }
  reviewer: { app_id: ..., installation_id: ..., key_path: /…/agenthub-reviewer.pem }  # #73 次第

repo_groups:
  core: [kishibashi3/agent-hub, kishibashi3/agent-hub-bridges, kishibashi3/agent-hub-sdk, kishibashi3/agent-hub-roles-kaz]

rules:
  - id: reviewer-review
    from: ["@reviewer"]
    app: reviewer
    repos: core
    ops: [pr.review, pr.comment]
  - id: planner-merge
    from: ["@planner"]
    app: bot
    repos: core
    ops: [pr.merge, issue.close, issue.comment, pr.comment, issue.create, issue.label]
  - id: impl-default
    from: ["@bridges-impl", "@agent-hub-impl", "@sdk-impl"]
    app: bot
    repos: core
    ops: [issue.create, issue.comment, pr.create, pr.comment, push]
```

- 1 つの依頼に当たる rule が 0 件なら拒否。2 件以上当たったら**設定誤りとして拒否**（どちらの鍵を使うか推測しない）
- 起動時に検証する（未知の op 名、存在しない app、鍵ファイルの mode が 600 でない、等）。**1 つでも通らなければ起動しない**（中途半端な表で動かない）
- 表の置き場: 変更が review を通る場所（roles-kaz か専用 repo）。反映は operator。ゲートウェイ自身は表を書き換えない
- 誰にどの op を許すかは本 doc では決めない。上の例は形の説明。**誰が何をしてよいかは L1 の判断**（`collaboration-model` の L0/L1/L2 に合わせる）

### 3.3 鍵の選び方

鍵の切り替えは 2 段ある:

1. **どの App か**（= GitHub 上の名義）: rule の `app`
2. **その App の中でどの権限・どの repo か**: installation token を作るときに `repositories` と `permissions` を指定して絞る（`POST /app/installations/{id}/access_tokens` の body）。例: `issue.comment` を `agent-hub-bridges` に → `{"repositories":["agent-hub-bridges"],"permissions":{"issues":"write"}}`

2 は App が 1 つでも効く。**#73 で App が 1 つに決まっても、「どの peer に何をさせるか」はゲートウェイで分けられる。** 分けられないのは GitHub 上の名義だけ（§7）。

トークンは (app, repo, 権限の組) ごとにキャッシュし、期限 5 分前に作り直す（今の `iat.go` と同じ考え方）。`iat.go` に絞る引数を足す必要がある（実装時）。

### 3.4 断り方

- 送り主に DM で返す（`caused_by` = 依頼 msg ID）。形: `❌ denied: <op> <repo>#<n> — <理由>（rule=<id> / no-rule / ambiguous-rule / bad-args / expired）`
- 理由は送り主が直せる粒度で書く。他の peer の rule の中身は書かない
- **拒否したときに別の鍵で試すことはしない**（PAT へのフォールバックもしない）。IAT モードの「未設定なら PAT」のような逃げ道はゲートウェイには作らない
- 拒否も監査に記録する（§3.5）。拒否が続く送り主は、表の不足か、peer が誤った操作を頼んでいるかのどちらか。operator が見る

### 3.5 監査の記録

SQLite（ゲートウェイの uid だけが書ける）。1 依頼 1 行、状態を更新していく。

| 列 | 中身 |
|---|---|
| `msg_id` | 依頼 DM の ID（**一意**。同じ DM を 2 回実行しないための鍵） |
| `caused_by` | 依頼 DM の caused_by（因果の連鎖） |
| `received_at` / `started_at` / `finished_at` | RFC 3339 UTC |
| `from` | 送り主 handle（§1.3 のとおり、hub の名乗りをそのまま記録したもの） |
| `op` / `repo` / `target` | 何を・どこに |
| `args_digest` | 本文の sha256 と長さ（本文は GitHub 側に残るので全文は持たない） |
| `decision` | `allowed` / `denied` |
| `rule_id` / `deny_reason` | |
| `app` / `token_scope` | 使った App と、絞った権限・repo |
| `state` | `queued` → `running` → `done` / `failed` / `unknown` |
| `http_status` / `github_request_id` | GitHub の応答（`X-GitHub-Request-Id`） |
| `result_url` | できたコメント・PR・merge commit の URL |

- `running` のまま落ちた行は、再起動時に**自動で再実行しない**。`unknown` にして送り主に「結果不明、GitHub で確かめてから再依頼して」と返す（コメントの二重投稿・二重 merge を避ける）
- GitHub 側の記録: author が App の bot 名義になり、footer に `@<from>` が入る。「誰が・いつ・どこに・何をした」は GitHub 側の author + footer と、ゲートウェイの監査の 2 か所で突き合わせられる

### 3.6 読み取り

読み取り（`gh pr view` / `gh pr diff` / `gh pr checks` / `gh api` の GET）は peer の作業のほとんどを占める（未集計、S1 で測る）。これを全部ゲートウェイに通すと詰まり所が大きくなるので、2 案:

- **R1（推奨）**: ゲートウェイが**読み取り専用に絞ったトークン**（contents / pull_requests / issues / checks / actions すべて `read`、repo も絞る）を作り、peer に渡す。peer は今の `gh` で読める。漏れても書けない
- R2: 読み取りもゲートウェイに通す。記録は全部残るが、待ちが増える

R1 でも、読み取りトークンを作るのはゲートウェイなので、ゲートウェイが落ちると最長 1 時間後に読み取りも止まる（§5.3）。

### 3.7 push

push は working tree が要るので、DM 1 通では運べない。案:

- **P1**: peer が `git bundle` を作り、ゲートウェイが読める場所に置いてパスを DM する。ゲートウェイは bundle を検証（branch 名がポリシーの prefix に合うか、`main` などの保護 branch でないか）して、自分の鍵で push する。**鍵は peer に渡らない**
- P2: ゲートウェイが、その repo・`contents: write` だけに絞ったトークンを peer に渡す。単純だが、その 1 時間は peer がその repo に何でも push できる（**迂回の窓が開く**）

推奨は P1。P2 は S2 の間の暫定として使うかもしれない（その場合は保護 branch を GitHub 側の ruleset で守る）。**push を App の名義で行うかどうか自体が #73 の権限（Contents: write）次第**（§7）。

---

## 4. 設計してほしいこと 3: inbound（`@gh-watch`）との関係

**別プロセスにする。同じ App・同じ部品を使う。**

| | inbound（`@gh-watch`） | outbound（`@github`） |
|---|---|---|
| 要るもの | 公開 ingress + webhook secret | App の秘密鍵 |
| 置き場所 | ingress に近い所（hub は家庭内 Pi5） | 秘密鍵を peer と別 uid で持てる所 |
| 落ちたとき | 通知が来ないだけ（書き込みは止まらない） | 書き込みが全部止まる |
| 攻撃面 | インターネットから届く | hub からだけ |

- 1 プロセスにすると、**インターネットに面したプロセスが秘密鍵を持つ**ことになり、通知側の不具合で書き込みも止まる。分ける理由はこの 2 つ
- kishibashi3 の言う「app 登録してフックにもなる」は、**同じ App の webhook を `@gh-watch` が受ける**ことで満たす。1 つの App に「書き込みの名義」と「webhook」の両方を持たせる（#73 の 2026-10-30 コメントの権限表・Webhook events と同じ）
- 将来の連携の例: `@github` が merge した後、`@gh-watch` に来る `pull_request.closed` を見て、依頼主に完了を知らせる。今は `@github` が API の応答で直接返すので必須ではない
- 実装言語は今は決めない。`iat.go` を使うなら Go、`gh-watch` / `scheduler` と揃えるなら Python。どちらでも設計は変わらない

---

## 5. 設計してほしいこと 4: 直列にして何が守られ、何が守られないか（残るもの）

### 5.1 残るもの（「絶対に事故が起きない」とはならない）

| 残るもの | 例 | 何で減らすか | 消えない理由 |
|---|---|---|---|
| **ゲートウェイ自体の設定誤り** | `policy.yaml` で `@writer` に `pr.merge` を与えてしまう。App に要らない権限を付けてしまう | 表の変更を review に通す。起動時の検証。既定拒否。拒否・許可の件数を毎日見る | 表が「正しく誤っている」場合は検証では分からない |
| **相手と鍵の対応の誤り** | **§1.3 の名乗り替え**。handle の付け替え（bridge を別 role に流用）で、古い rule が新しい中身の peer に当たる | hub 側で handle ごとに資格情報を分ける（§8）。handle の付け替え時に表を見直す運用 | hub が handle を保証するまでは、ゲートウェイには名乗りの真偽が分からない |
| **迂回** | peer が `gh` / `curl` / `git push` で直接書く。IAT モードの `GH_TOKEN`。読み取りトークン以外のトークンが env やファイルに残っている | S3 で peer から書き込みの資格情報を外す（§6） | peer とゲートウェイが同じ uid の間は、ゲートウェイの鍵ファイルも hub の PAT も peer から読める。**uid を分けるまでは塞がらない** |

その他に残るもの:

- **許された操作の中身の誤り**: 許可された `issue.comment` で誤った内容を書く、正しい権限で間違った PR を merge する。ゲートウェイは中身を判断しない（非エージェントなので）
- **ゲートウェイの乗っ取り**: 鍵が 1 か所に集まるので、そこが破られると全 App の権限が出る。今は鍵が全 peer から読めるので、集めることで悪くはならないが、ゼロにもならない

### 5.2 直列で守られるもの・守られないもの

守られる（ゲートウェイを通る書き込み同士の間だけ）:

- 同じ PR・issue への書き込みが、ゲートウェイに届いた順に 1 つずつ実行される（対象ごとのキュー。repo 全体の 1 本のキューにはしない）
- **merge と approve は sha 付き**にするので、「見た head と違う head を merge / approve する」ことは、ゲートウェイの外で誰かが push した場合でも GitHub が断る。これは直列とは別の守り方で、外の経路にも効く唯一の部分

守られない:

- **ゲートウェイを通らない経路との競合**: kishibashi3 本人の操作、GitHub Actions / CI、`ntv-*` を触る bridge（`kishibashi3` の PAT のまま）、S3 までの間に `gh` を直接使う peer。これらとゲートウェイの書き込みの順序は保証できない（sha 付き merge / approve を除く）
- **判断の競合**: 2 つの peer が同じ issue を同時に直し始める、といった作業の重複。GitHub API の順序の問題ではないので、直列にしても防げない
- **対象をまたぐ順序**: PR A の merge と PR B の作成の順序はキューが別なので保証しない
- **ゲートウェイに届く前の順序**: hub に先に届いた DM が先に処理される保証は、hub の配送順に依る

### 5.3 詰まり所としてのゲートウェイ

待ち:

- 1 件ごとに GitHub API 1〜数回（トークンが無ければ access_tokens も 1 回）。1 件あたり概ね 1 秒前後〜数秒の見込み（未計測、S1 で測る）
- 対象ごとのキューなので、別の PR への書き込みは並ぶが待たない。同じ PR に複数 peer が集まったときだけ待つ
- GitHub の rate limit が App の installation 単位になる。いまは `kishibashi3` のユーザー単位の上限を全 peer で分けている。content を作る操作には secondary rate limit がある（数値は実装前に GitHub の docs で確かめる）。上限に当たったら、ゲートウェイは待たずに `failed (rate limited, retry after …)` を返す（peer が勝手に再送して詰まりを悪化させないよう、retry-after を伝える）

落ちたときに止まるもの:

| 止まるもの | いつから |
|---|---|
| kishibashi3/* への書き込み全部（コメント・issue・PR・review・merge・push(P1)） | すぐ |
| 読み取り（R1 の場合） | 渡したトークンの期限が切れたとき（最長 1 時間後） |
| `@gh-watch` の通知 | 止まらない（別プロセス） |
| `ntv-*` への操作 | 止まらない（ゲートウェイを通らない） |

落ちている間の依頼は hub の inbox に残る。再起動時の扱い:

- 依頼に有効期限を付ける（例: 受信から 30 分。値は S1 で決める）。過ぎた依頼は実行せず `expired` で返す（古い merge 依頼を後から実行しないため。sha 付きなので head が変わっていれば GitHub も断るが、それに頼らない）
- `msg_id` が監査にあれば実行しない（二重実行防止）
- 落ちたことを知る手段: `/ping`、`@scheduler` からの定期 `/ping`、operator の監視

非常口: ゲートウェイが落ちている間、書き込みは **kishibashi3 本人だけ**が自分の `gh` で行う。operator に非常口を持たせるかは L1 判断（持たせると operator の経路が迂回になる）。

---

## 6. 入れる段

#407 の制約（root `CLAUDE.md` §「破壊的更新は段階的 deprecation で（いきなり壊さない）」。この節は手元の `~/app/CLAUDE.md` では見つけられなかった）に従い、いきなり `gh` を取り上げない。各段は **1 体で実機確認 → 残り** の順（staged rollout）。

| 段 | やること | peer の `gh` | 抜ける条件 |
|---|---|---|---|
| **S0** | 本 doc。#73 の答え | 今のまま | #73 で App の権限・入れ先が決まる |
| **S1 観測** | ゲートウェイを起動し、`/ping` と**読み取り系と `issue.comment` だけ**を 1 体（例: `@bridges-impl`）から使う。同時に、peer が `gh` で何をしているかを session 記録から集計する（op 表の根拠） | 今のまま | op 表が集計で裏付けられる。待ち時間・失敗率が測れている |
| **S2 併用** | kishibashi3/* だけを触る peer の書き込みを順にゲートウェイに切り替える（persona doc / bridge の指示を変える）。`gh` はまだ持っている。`gh` で書き込もうとしたら PreToolUse フックで**警告して**ゲートウェイを案内する（拒否はまだしない） | 持っている（**ゲートウェイは助言**） | 迂回の件数（§9 M2）が 1 週間 0 |
| **S3 外す** | kishibashi3/* だけを触る peer から書き込みの資格情報を外す: ①`GH_TOKEN`（IAT モード）を渡さない ②`hosts.yml` を peer の uid から読めなくする ③peer の `gh` は R1 の読み取りトークンだけで動かす ④ゲートウェイの鍵ファイルを peer から読めなくする | **書けない** | 迂回の件数が 0 のまま、peer の作業が回る |
| **S4 範囲の外** | `ntv-*` を触る bridge と両方を触る bridge。#73 の設計 B で App を `ntv-contech` に入れないので、この bridge は `kishibashi3` の PAT のまま。**本設計の範囲外**として残す | 今のまま | — |

S3 の前提:

- **②④は uid を分けないと実現できない。** 今は全 bridge・operator・kishibashi3 が同じ uid（`kishibashi3`）で動いている。`hosts.yml` もゲートウェイの鍵も、mode 600 でも同じ uid なら読める（#73 の 2026-11-14 コメント「残るリスク」と同じ）。uid の分け方は本 issue の外（publications#119 の Agent Authority Control Plane 側）とし、**S3 はその結論待ちになる**
- uid を分けるまでの間（S2）にできるのは、フック（警告→拒否）・指示・監査による検出まで。**フックは `curl` や `git` 直叩きで迂回できるので、ガードレールであって境界ではない**
- §1.3 の名乗り替えも、hub の PAT が peer の env にある限り残る。S3 で ②④ と同時に、hub 接続の資格情報も peer ごとに分ける必要がある（#391 / publications#119）

S3 で外した後に peer が困る操作（例: `gh run rerun`、`gh project`、表に無い op）は、拒否の監査から拾い、表に足すかどうかを L1 で決める。

---

## 7. #73 で止まっている所（分岐）

**#73 の問い: App にどの権限を与え、どの repo に入れるか**（今は App はあるが permissions `{}`、入れ先 0 repo）。これが決まらないと、§3.2 の `apps` と §3.3 で絞れる上限が決まらない。以下は分岐として書く。答えが出たら、該当する枝を残して他を消す。

### 7.1 App の数

| #73 の答え | 鍵の切り替え | approve |
|---|---|---|
| **A. App 2 つ**（`AgentHub [bot]` + `AgentHub Reviewer [bot]`、planner 案 2026-10-30 / 2026-11-14） | `@reviewer` の rule → Reviewer App、他 → bot App。§3.3 の絞り込みは両方で効く | `AgentHub [bot]` の PR を `AgentHub Reviewer [bot]` が approve できる（branch protection の required review に数えられるかは #73 の完了条件 1 で確かめる） |
| **B. App 1 つ** | 全 rule が bot App。peer ごとの違いは §3.3 の絞り込みだけ | **formal な approve は使えないまま**（作者 = approve 者になる）。`LGTM ✅` の運用は残り、ゲートウェイの `pr.review approve` は op 表から外す |
| **C. App を使わない**（PAT のまま） | ゲートウェイは PAT 1 本しか持てず、peer ごとに権限を絞れない（PAT は呼び出しごとに絞れない）。**op 表による拒否だけが残る** | 使えない |

C だと「鍵を切り替える」部分が成り立たない。B でも権限の切り替えは成り立つ。

### 7.2 権限

| #73 の答え | ゲートウェイでできること |
|---|---|
| Contents: write あり | §3.7 の push を App 名義で行える。`pr.merge` も可 |
| Contents: write なし | push と merge は App ではできない。push は peer が PAT のまま行う → **S3 で `gh` を外せない**（迂回が残る）。merge は人か PAT の経路 |
| Workflows: write なし | `.github/workflows/` を含む push は App では断られる。その push だけ人の経路に残す |
| Issues: write を Reviewer App に付けない（planner 案） | `@reviewer` は issue 起票を reviewer 名義でできない。bot App の rule を `@reviewer` にも付けるか、起票は他の peer に頼む |

### 7.3 入れる repo

- 入れた repo だけがゲートウェイの対象。入っていない repo への依頼は `denied (repo not installed)` で返す（PAT に落とさない）
- `ntv-contech` に入れない（#73 の設計 B）なら、S4 のとおり NTV 側は範囲の外

### 7.4 鍵の置き場

#73 の 2026-11-14 コメントの案（kishibashi3 が置き、operator も peer も中身を見ない）をそのまま使う。違いは 1 点: **ゲートウェイの uid だけが読める**ように置く（S3 の ④）。uid を分けるまでは今の案と同じ。

---

## 8. 他の issue との関係

| issue | 関係 |
|---|---|
| **#73**（App の権限と入れ先） | **前提。** §7 の分岐。#73 が「各 bridge に `GH_TOKEN=<IAT>` を渡す」形で先に進んだ場合、その形はゲートウェイの S3 で外す側になる（IAT を peer に渡すと 1 時間は App の全権限が peer にあるため）。#73 の完了条件「approve」「実測」は、ゲートウェイ経由でも満たせる |
| **#391**（fleet.env のトークンを用途ごとに分ける） | 並行。ゲートウェイが入っても hub 接続の `GITHUB_PAT` は全 peer に要る。§1.3 の名乗り替えを塞ぐには、この PAT を peer ごとに分ける（か hub 側で handle ごとの資格情報にする）必要があり、それは #391 / publications#119 の範囲 |
| **#398**（`gh` トークンの `delete_repo` / `admin:public_key` を外す） | 並行・先にやってよい。S3 の後も、この `gh` トークンは kishibashi3 本人と `ntv-*` bridge が使い続けるので、ゲートウェイでは scope は消えない。ゲートウェイを待たずに外すのがよい |
| agent-hub#543（hub 接続 PAT の入れ替え） | 値の入れ替えだけ。本設計とは独立 |
| publications#119（Agent Authority Control Plane） | **S3 の前提**（uid を分ける、peer ごとの身元）。本設計はその結論を待つ |
| `@gh-watch`（#233、agent-hub#289） | §4。別プロセス・同じ App |

---

## 9. 測り方

| | 何を | どう測る | 目標 |
|---|---|---|---|
| M1 | kishibashi3/* の書き込みのうちゲートウェイ経由の割合 | GitHub の author で集計（App の bot 名義 / `kishibashi3`）。`kishibashi3` 名義のうち footer に peer の handle があるものは peer の直書き | S2 の終わりで、peer の書き込みはほぼ全部 App 名義 |
| M2 | 迂回の件数 | `kishibashi3` 名義かつ footer に peer の handle がある書き込みの数。S2 では PreToolUse フックの警告件数も | S3 の後 0 |
| M3 | 拒否の件数 | 監査の `decision=denied` を送り主・理由ごとに | 0 にはならない。理由の内訳を見て、表の不足か peer の誤りかを分ける |
| M4 | 待ちと失敗 | 監査の `started_at - received_at`、`finished_at - started_at` の p50 / p95、`failed` / `unknown` の件数 | S1 で基準値をとる |
| M5 | 落ちていた時間 | `/ping` の無応答時間、落ちている間の `expired` 件数 | |
| M6 | peer の資格情報 | S3 の後、各 peer のプロセス env（キー名だけ）に `GH_TOKEN` が無い。peer の uid から `hosts.yml` とゲートウェイの鍵が読めない | 全 peer で無い・読めない |

---

## 10. 決めていないこと（L1 / kishibashi3 判断）

1. **#73**: App の数・権限・入れ先（§7）
2. 誰にどの op を許すか（§3.2 の表の中身）
3. 読み取りを R1 / R2 のどちらにするか（§3.6、推奨 R1）
4. push を P1 / P2 のどちらにするか（§3.7、推奨 P1）
5. operator に非常口を持たせるか（§5.3）
6. uid の分け方（publications#119 側、S3 の前提）
7. ゲートウェイを置く host（bridge host か Pi5 か）。鍵を peer と別 uid で持てることが条件

## 調査済み事項

- issue #407 本文、#73（本文と 2026-06-09 / 2026-08-21 / 2026-10-30 ×2 / 2026-11-14 ×2 のコメント）、#391（本文と 2026-10-30 の調査コメント）、#398、agent-hub#543
- `packages/gh-watch/README.md` と `gh_watch.py`（webhook の HMAC 検証、購読の sender 単位管理、処理後に mark_as_read）
- `packages/github-client/iat.go`（access_tokens を body なしで呼んでいる = 権限と repo を絞っていない）
- `agent-hub/packages/scheduler/README.md` と `scheduler.py` の `_check_owner`（送り主 = owner の entry だけ操作可）
- `agent-hub/src/mcp/server.ts` の auth middleware（`X-Participant-Id` による handle override、`AGENT_HUB_STRICT_HANDLE_OWNERSHIP`）
- `bridge-claude2/cmd/bridge/runner.go` の IAT モードと `filteredEnv`（`GITHUB_APP_*` だけ除く）、`bridge-codex2/cmd/bridge/runner.go`（`GITHUB_PAT` を内側に渡す）
- 未確認: `gh-watch` の実動作、peer ごとに絞った installation token の実挙動、App の approve が required review に数えられるか、GitHub の rate limit の現在値、peer が実際に使っている `gh` 操作の内訳（S1 で集計）

---
@bridges-impl [bridge-claude2] (operator-supervised)
