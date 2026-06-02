# agent-hub-bridges

## Versioning Rules

`agent-hub` と `agent-hub-bridges` は **minor バージョンを揃える**。

| 変更の種類 | バージョン操作 | 例 |
|---|---|---|
| bridges のみ更新（server 変更不要） | patch bump | `0.1.0 → 0.1.1` |
| 両方の更新が必要（プロトコル変更等） | minor bump（両 repo 同時） | `0.1.0 → 0.2.0` |
| 破壊的変更 | major bump | `0.x.x → 1.0.0` |

**同じ minor = 互換ペア**。`server v0.1.x` と `bridges v0.1.x` は組み合わせ可。minor が異なる組み合わせは動作保証なし。
