<!-- source: README.md synced-through: 4f1be83 -->
> **[English](../../../README.md)** | **[简体中文](../zh-CN/README.md)** | **日本語** | **[한국어](../ko/README.md)**

<p align="center">
  <img src="../../images/herodemo.gif" alt="ATLAS TUI 動作中"/><br/>
  <sub><i>ATLAS TUI のライブデモ（10倍速）。V3 パイプラインがファイル作成を実行中。</i></sub>
</p>

<h1 align="center">A.T.L.A.S.</h1>
<p align="center"><b>Adaptive Test-time Learning and Autonomous Specialization</b></p>

<p align="center">
  <img src="https://img.shields.io/badge/version-V3.1.4-blue" alt="Version"/>
  <img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="License"/>
  <img src="https://img.shields.io/badge/model-agnostic-green" alt="Model-agnostic"/>
</p>

<p align="center">
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/test.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/test.yml?branch=main&label=tests" alt="Tests"/></a>
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/install-test.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/install-test.yml?branch=main&label=install%20matrix" alt="Install matrix"/></a>
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/codeql.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/codeql.yml?branch=main&label=codeql" alt="CodeQL"/></a>
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/container-scan.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/container-scan.yml?label=container%20scan" alt="Container scan"/></a>
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/verify-tags.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/verify-tags.yml?label=release%20signature" alt="Release signature"/></a>
</p>


## 🌎 ATLAS とは

**ATLAS は、フロンティアモデル級の推論と検証をコンパクトなオープンモデルにもたらす、ローカルで動くコーディングエージェントです。** モデルを取り巻くシステム側（プランニング、候補生成、品質スコアリング、サンドボックスでのテスト、修復）により多くの知性を持たせることで、小さなモデルでも実際のソフトウェア開発の仕事を、ホスト型 API にもトークン課金にも頼らず、すべて自分のハードウェア上でこなせるようにします。

## 💡 なぜ ATLAS なのか

* **小さなモデルからより多くを引き出す。** ATLAS は単発の生成に依存する代わりに、プランニング、候補選択、検証、修復をモデルの周りに重ねます。
* **受け入れる前に検証する。** 生成されたコードは、分離された実行環境の中でコンパイル・テスト・修正できます。
* **計算資源を要所に集中させる。** 単純な編集は短いパスで済ませ、難しいタスクにはより多くの候補・推論・検証を割り当てます。
* **自分のモデルを動かす。** NVIDIA、AMD、Apple Silicon、Vulkan、あるいは CPU 対応ハードウェア上で、互換性のある GGUF モデルを使えます。
* **ローカルで管理する。** ATLAS は、リポジトリやプロンプトをホスト型モデルまたは ATLAS 運営サービスへ意図的にアップロードしません。サンドボックスコマンドはデフォルトで外部ネットワークへアクセスできます。無効にするには `ATLAS_SANDBOX_NET_INTERNAL=true` を設定してください。
* **スタック全体を所有する。** ATLAS はオープンソースかつセルフホストです。ホスト型モデルやサードパーティのモデルプロバイダー API キーは不要で、ローカルのインストール単位のサービストークンが ATLAS サービス間を認証します。

---

## 📰 最新ニュース

- **2026-07-06** - **[V3.1.3 "Maia" リリース](https://github.com/itigges22/ATLAS/releases/tag/v3.1.3)** - 本番プラットフォーム強化: 自動復元付きの段階的アップグレード/ロールバック、SQLite ステートストア（Redis を廃止）、署名付きアーティファクトマニフェスト、相関 ID 付き構造化ログ、対話式パーミッション、セッション再開、そして2回の敵対的バグ修正スイープ
- **2026-06-17** - **[V3.1.2 "Maia" リリース](https://github.com/itigges22/ATLAS/releases/tag/v3.1.2)** - ハードウェア対応の拡大（ROCm / Metal / Vulkan）、持ち込みモデルの Lens + ASA トレーニング、自分のワークロードからのインザループ lens 再トレーニング（その後削除。CHANGELOG を参照）、エージェント信頼性の強化
- **2026-05-12** - **[V3.1.0 "Maia" リリース](https://github.com/itigges22/ATLAS/releases/tag/v3.1.0)** - ネイティブ Bubbletea TUI、ワンコマンドブートストラップ、ストリーミング Lens + ASA 活性化ステアリング、AST 対応の外科的編集
- **2026-03-26** - [Hacker News フロントページ](https://news.ycombinator.com/item?id=47533297) - 489 ポイント、285 コメント
- **2026-03-05** - **V3.0 リリース** - 凍結された Qwen3-14B 上のマルチフェーズ V3 パイプライン。*このリリースで公表した LiveCodeBench 74.6% は撤回しました。ベンチマークランナーは LiveCodeBench の非公開テストを一度も実行しておらず、3 つの候補のいずれか、または失敗出力を見せた修復が問題文の例に合格すればタスクを合格としていました（[撤回のお知らせ](../../reports/V3_ABLATION_STUDY.md)）。現行製品の再検証後に改めて測定します。*
- **2026-02-18** - **[V2.0 リリース](../../../CHANGELOG.md)** - ベンチマークインフラ、HumanEval/MBPP/LiveCodeBench/GPQA/SciCode 評価スイート

## ⭐ Star History

<!-- Self-hosted chart: rendered weekly by .github/workflows/star-chart.yml
     onto the `star-history` asset branch (scripts/star-history-chart.py).
     Replaces the star-history.com embed, whose shared token pool
     rate-limits unpredictably. -->
<a href="https://github.com/itigges22/ATLAS/stargazers">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/itigges22/ATLAS/star-history/star-history-dark.svg" />
   <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/itigges22/ATLAS/star-history/star-history-light.svg" />
   <img alt="Star history chart" src="https://raw.githubusercontent.com/itigges22/ATLAS/star-history/star-history-light.svg" width="100%" />
 </picture>
</a>

<sub>毎週月曜日に GitHub Actions で更新されます。</sub>

---

## 🧱 ATLAS の機能

1. **[atlas-tui](../../CLI.md)** - ネイティブ Bubbletea ターミナル UI。公式チャットクライアント。任意のプロジェクトディレクトリで `atlas` と入力すれば起動します。
   - [ライブパイプライン表示](../../CLI.md#panes) - V3 ステージをサイドペインで監視
   - [スラッシュコマンド](../../CLI.md#slash-commands) - `/add`、`/diff`、`/commit`、`/run` でローカルファイルとシェルを操作
   - [入力モード](../../CLI.md#input-modes) - チャット、`!bash`、`/slash` をヒントドロップダウン付きで切り替え

2. **[atlas-proxy](../../ARCHITECTURE.md#3-atlas-proxy-outer-layer)** - システム全体を統括する Go 製エージェントループ。
   - [ツールコールルーティング](../../ARCHITECTURE.md#tools) - ファイル操作を複雑度ティアで分類
   - [文法強制](../../ARCHITECTURE.md#grammar-enforcement) - GBNF スキーマで期待される JSON 形式へ強く誘導し、不正または切り詰められた出力はプロキシ側で回復
   - [BiasBusters](../../ARCHITECTURE.md#tool-selection-bias-mitigations) - 構造的なコード編集でモデルを `structural_edit` へ誘導する4つの複合的な緩和策（説明文、文法禁則、システムノート、ASA ステアリング）
   - [安全制限](../../ARCHITECTURE.md#safety-limits) - ターン上限、トークン予算、タイムアウト

3. **[V3 パイプライン](../../ARCHITECTURE.md#4-v3-pipeline-inner-layer)** - 単一のプロンプトを検証済み候補に変換するマルチフェーズコード生成。
   - [PlanSearch](../../ARCHITECTURE.md#pipeline-flow) - 制約駆動の構造化プランニング
   - [DivSampling](../../ARCHITECTURE.md#pipeline-flow) - 温度と戦略をまたぐ多様な候補生成
   - [予算ティア](../../ARCHITECTURE.md#pipeline-flow) - アロケータがタスクごとに計算ティアを選ぶ。現状、ティアが決めるのは候補数であり、思考の深さではない
   - [PR-CoT Repair](../../ARCHITECTURE.md#pipeline-flow) - サンドボックスのエラー出力をもとに失敗した候補を修復
   - [Refinement Loops](../../ARCHITECTURE.md#pipeline-flow) - サンドボックスでの検証と修正を繰り返す

4. **[Geometric Lens](../../ARCHITECTURE.md#5-geometric-lens)** - モデル自身の埋め込み上で動くエネルギーベースのスコアリング。外部オラクル不要。(「[Geometric Lens とは?](../../ARCHITECTURE.md#why-geometric-lens)」)
   - [C(x) Cost Field](../../ARCHITECTURE.md#scoring-models) - 候補の品質をスコア化する、モデルの隠れ次元→512→128→1 の MLP
   - [G(x) Quality Prediction](../../ARCHITECTURE.md#scoring-models) - 選択に用いる XGBoost アンサンブル
   - [RAG / PageIndex V2](../../ARCHITECTURE.md#rag--pageindex-v2) - AST 対応のコード検索とプロジェクトインデキシング
   - [Confidence Router](../../ARCHITECTURE.md#confidence-router--pattern-cache) - Thompson Sampling で必要な候補に計算を寄せる

5. **[Sandbox](../../ARCHITECTURE.md#6-sandbox)** - ビルド検証のための分離実行環境。
   - 多言語実行: Python、Rust、Go、C、Shell など
   - スコアリング前のコンパイルとリント
   - 生成テストと既存テストスイートの両方を実行

6. **[llama-server](../../CONFIGURATION.md#6-llama-server)** - 単一のコンシューマ GPU 上でのローカル LLM 推論。
   - GPU 加速の量子化推論 (Q6_K / Q4_K_M) — NVIDIA CUDA、AMD ROCm、Apple Metal (macOS ハイブリッド)、Vulkan に対応。Intel SYCL はロードマップ
   - トークンレベルの文法制約デコーディング
   - セルフ埋め込み（レンズのための別モデルは不要）

詳細ドキュメント（セットアップ、アーキテクチャ、設定、トラブルシューティング、ベンチマークレポート、各コンポーネントの[研究的背景](../../SOURCES.md)）は [docs/](../../) にあります。

---

## 🚀 はじめに

ワンショットインストール:
```bash
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

変化し続けるスクリプトをそのまま bash にパイプしたくない場合は、同じインストーラーをより慎重に実行する方法が2つあります:
```bash
# Pinned to a release: script, checkout, and images all at the signed tag
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/v3.1.3/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.3 bash

# Review before running
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

スクリプトはディストロ (Ubuntu、Debian、RHEL、Fedora、Rocky、Alma) と GPU ベンダー (NVIDIA → nvidia-container-toolkit; AMD → ROCm デバイスパススルー) を判定し、適切なランタイムをインストールし、モデル重みをダウンロードし、ASA ステアリングベクトルをビルドしてスタックを起動します。所要時間は 10〜30 分程度で、ボトルネックはモデルのダウンロードです。

完了後、任意のプロジェクトディレクトリで `atlas` を実行してください。

**要件**

| | |
|---|---|
| GPU | VRAM 16GB 以上。NVIDIA (CUDA、サポート対象 (Supported))、AMD (ROCm、コミュニティ検証済み (Community-tested))、または Apple Silicon (Metal、macOS ハイブリッド、サポート対象)。その他大半の GPU は Vulkan (プレビュー (Preview)) でカバー。プレビルドの CUDA イメージは Blackwell (RTX 50xx) を対象としており、それより古い NVIDIA GPU は一度だけのローカル再ビルドが必要です ([SETUP.md § CUDA Compute Capability](../../SETUP.md#cuda-compute-capability-dockerfilev31) を参照)。レベルの定義: [SUPPORT_MATRIX.md](../../../SUPPORT_MATRIX.md)。GPU 一覧: [SETUP.md § Supported GPUs](../../SETUP.md#supported-gpus)。特定のモデルがお使いのカードに収まるかは [What fits on my GPU?](../../TROUBLESHOOTING.md#what-fits-on-my-gpu) を参照。 |
| ランタイム | Docker (NVIDIA: + nvidia-container-toolkit; AMD: 単体の Docker で十分) または Podman |
| Python | 3.9 以上 |
| ディスク | 約 20GB CUDA / 約 22GB ROCm (モデル重み + コンテナイメージ) |

Apple Silicon は macOS ハイブリッド Metal パス（ネイティブ llama-server + 残りは Docker — **[SETUP_MACOS.md](../../SETUP_MACOS.md)** を参照）でネイティブ動作します。Intel Arc (SYCL) はロードマップ上の項目です。手動インストール手順 (Docker Compose、ベアメタル、K3s) とブートストラップフラグの一覧は **[SETUP.md](./SETUP.md)** をご参照ください。

---

## ⚠️ 既知の制限事項

- **Linux の Docker スタック、加えてネイティブ macOS パス。** NVIDIA (サポート対象 (Supported))、AMD ROCm (コミュニティ検証済み (Community-tested))、Vulkan (プレビュー (Preview)) の Docker パスが現在存在します。Apple Silicon (サポート対象) はネイティブ macOS ハイブリッド Metal パス ([#32](https://github.com/itigges22/ATLAS/issues/32)) で動作します。Intel Arc / SYCL はロードマップ (Roadmap) です。レベルの定義: [SUPPORT_MATRIX.md](../../../SUPPORT_MATRIX.md)。
- **ATLAS には現在のベンチマーク結果がありません。** V3.0 の LiveCodeBench の数値は撤回しました（最新ニュース参照）。現行リリースの能力・信頼性の数値はまだ測定されていません。数値は保留タスクでの再測定後にのみ公表し、モデル別の結果は [#28](https://github.com/itigges22/ATLAS/issues/28) で追跡します。
- **複雑な機能追加は不安定なことがあります。** コンパクトなモデルは、コードを書き始める前に不慣れなコードベースの探索にエージェントターンを費やすことがあります。
- **文法制約デコーディングは制約なしのデコーディングより遅くなります。**

---

## 🗺️ ロードマップ

**V3.1.3 "Maia"** - 現在のリリース。V3.1.2 の上に本番プラットフォーム強化を実施: 自動復元付きの段階的な `atlas upgrade`/`rollback`、Redis を置き換える SQLite ステートストア ([ADR 0007](../../adr/0007-sqlite-state-store.md)。2026-09 に `dev` で、唯一の利用者だったパターンキャッシュとともに廃止)、署名付きアーティファクトマニフェスト、サービス横断の相関 ID を持つ構造化 JSON ログ、対話式パーミッションプロンプト、セッション再開、型付き設定のバリデーション/マイグレーション、2回の敵対的バグ修正スイープ（確認済み修正 33 件）。

**V3.1.2 "Maia"** - V3.1.0 の基盤（TUI、ワンコマンドインストール、ストリーミング Lens + ASA）の上に、ハードウェア対応の拡大、持ち込みモデルのトレーニング、エージェント信頼性の強化を実施。
- ハードウェア対応: llama.cpp 経由の AMD ROCm — RDNA4 / RX 9070 (gfx1200/gfx1201) を含む ([#26](https://github.com/itigges22/ATLAS/issues/26))。Apple Silicon のネイティブ macOS ハイブリッド Metal パス ([#32](https://github.com/itigges22/ATLAS/issues/32)、[SETUP_MACOS.md](../../SETUP_MACOS.md) を参照)。AMD / Intel / Snapdragon / MoltenVK 経由の Apple / CPU をカバーする Vulkan ユニバーサルフォールバック ([#114](https://github.com/itigges22/ATLAS/issues/114))。
- 持ち込みモデル: ローカル Lens トレーニングパイプライン (`atlas lens build`、[#100](https://github.com/itigges22/ATLAS/issues/100)) と ASA のモデル別キャリブレーション同等化 (`atlas asa check/build/publish`、[#113](https://github.com/itigges22/ATLAS/issues/113)) — 追加の GGUF 向けに Lens + ASA アーティファクトをトレーニングし、lens に同梱されるモデル別の動作閾値付きで出荷。
- エージェント信頼性: ツール結果の可視性修正、読み取り重複排除、トレースバック → 指向的編集、`move_file`、pip インストール / 大文字小文字不一致のステア、サンドボックスシェルポリシー + ホストサイズの cgroup 制限。
- 構造的な呼び出しグラフ推論 ([#39](https://github.com/itigges22/ATLAS/issues/39) / [#125](https://github.com/itigges22/ATLAS/pull/125)、[@yogthos](https://github.com/yogthos) に感謝)。ARCHITECTURE.md の zh-CN / ja / ko 翻訳 ([#25](https://github.com/itigges22/ATLAS/issues/25))。

**V3.2** - 次のマイルストーン: より深いコード推論とプランニング。
- アーキテクチャ優先のプランニングフェーズ — RPG スタイルのプラン先行・後埋め: モジュールスコープでプランを立ててから関数スコープで実装 ([#120](https://github.com/itigges22/ATLAS/issues/120)、PR [#124](https://github.com/itigges22/ATLAS/pull/124))。
- 構造的コード推論（残り） — ソルバー支援の到達可能性に加え、「どのファイルが重要か」を多解像度で検索する構文非依存のウェーブレット分解 ([#39](https://github.com/itigges22/ATLAS/issues/39))。
- サンプリングを用いた推論 — 効率と品質の向上 ([#9](https://github.com/itigges22/ATLAS/issues/9))。
- 先送りしたインフラ: 自動化された HuggingFace 投稿パイプライン ([#102](https://github.com/itigges22/ATLAS/issues/102))。K3s / Kubernetes 上の ROCm。レジストリモデルの正式ベンチマーク — LiveCodeBench、GPQA Diamond、SciCode ([#28](https://github.com/itigges22/ATLAS/issues/28))。

**バックログ / 協力者募集**
- ハードウェア: ARM64 マルチアーキテクチャビルド ([#115](https://github.com/itigges22/ATLAS/issues/115))、大規模モデル向けのマルチ GPU ([#34](https://github.com/itigges22/ATLAS/issues/34))、Intel oneAPI / SYCL ([#27](https://github.com/itigges22/ATLAS/issues/27))。
- ツール: VS Code / JetBrains 拡張機能 ([#35](https://github.com/itigges22/ATLAS/issues/35))。
- サンドボックス言語: Java / Kotlin ([#29](https://github.com/itigges22/ATLAS/issues/29))、Ruby / PHP ([#30](https://github.com/itigges22/ATLAS/issues/30))。
- アーキテクチャ: モデル非依存プラットフォーム ([#66](https://github.com/itigges22/ATLAS/issues/66))。

---

## ❤️ ATLAS を支援する

ATLAS は、一人の大学生が自由時間に、1枚のコンシューマ GPU の上で開発しています（[ATLAS の背景ストーリー](../../STORY.md)）。このプロジェクトが役に立っていて、持続可能な形で続いてほしいと思っていただけたら、ぜひ **[GitHub でのスポンサー](https://github.com/sponsors/itigges22)** をご検討ください。

スポンサーシップは以下に直接充てられます:

- **計算資源とハードウェア** - ベンチマークの反復を速めるための GPU 追加、メンテナーが自費では手の届かないアーキテクチャ（AMD ROCm、より大容量の VRAM カード、大規模モデル実験のためのクラウドレンタル）へのアクセス。
- **コントリビューターへの報奨金** - 実のある PR に本気で時間を割いてくれる外部コントリビューターへの相応の報酬。ATLAS が一人分のペースを超えて成長できるように。
- **研究** - このアーキテクチャを巡る継続的な学術活動。今後のワークショップ・学会への投稿から、アプローチを検証し拡張する論文執筆や共同研究まで。
- **コミュニティ** - ATLAS が動作するコミュニティとプラットフォームへの継続的なサポート。ドキュメント、ユーザー向けチャンネル、より多くの開発者に ATLAS を届け、既存ユーザーにより良く役立つための教育コンテンツを含みます。

すべてのスポンサーは、支援いただいたバージョンのリリースノートにクレジットされます。

---

## 🤝 コントリビュート

ATLAS はオープンに開発されており、コントリビューターとコアメンテナーを歓迎します。バグ修正、アクセラレータサポート、より大きなサブシステムの作業、いずれも歓迎です。

バグを見つけた、あるいは行き詰まった? **[Issue を作成してください](https://github.com/itigges22/ATLAS/issues)** — 修正の提出は必須ではありません。バグ報告とフィードバックはコードと同じくらい役に立ちます。

ガイドラインは **[CONTRIBUTING.md](../../../CONTRIBUTING.md)** を、コードベースのレイアウトの概要は[リポジトリマップ](../../MAP.md)をご覧ください。

---

## 📄 ライセンス

[GNU Affero General Public License v3.0 (AGPL-3.0)](../../../LICENSE) の下でライセンスされています。
