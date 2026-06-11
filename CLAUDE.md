# agent-hub-bridges

## Versioning Rules

`agent-hub` と `agent-hub-bridges` は **minor バージョンを揃える**。

| 変更の種類 | バージョン操作 | 例 |
|---|---|---|
| bridges のみ更新（server 変更不要） | patch bump | `0.1.0 → 0.1.1` |
| 両方の更新が必要（プロトコル変更等） | minor bump（両 repo 同時） | `0.1.0 → 0.2.0` |
| 破壊的変更 | major bump | `0.x.x → 1.0.0` |

**同じ minor = 互換ペア**。`server v0.1.x` と `bridges v0.1.x` は組み合わせ可。minor が異なる組み合わせは動作保証なし。

## 変更着手前の依存性確認（必須）

breaking change（リネーム / API 変更 / フラグ変更 / プロトコル変更）を行う前に、以下を必ず実施する。

- **影響を受ける全 repo・全コンポーネントを列挙する**（agent-hub server / bridges / sdk / roles-kaz など）
- 「揃えないと壊れるものが他にあるか」を確認してから着手
- 片側だけ変えて壊れる場合は、**互換レイヤーを先に入れるか、全部同時に変えるかを設計してから実装**
- 確認した内容を issue の「調査済み事項」に明記

> 背景: 2026-06-11 の v0.3.0 インシデントで「変更前に依存する全コンポーネントを確認しなかった」が根本原因だったため追記（operator 指示）。
