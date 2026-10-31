<!-- source: README.md synced-through: 04200038b5c2bd17d531f7441507853132f165aa -->
> **[English](../../../README.md)** | **[简体中文](../zh-CN/README.md)** | **日本語** | **[한국어](../ko/README.md)**

<p align="center">
  <img src="../../images/herodemo.gif" alt="ATLAS TUI 動作中"/><br/>
  <sub><i>ATLAS TUI のライブデモ（10倍速）。V3 パイプラインがファイル作成を実行中。</i></sub>
</p>

<h1 align="center">A.T.L.A.S.</h1>
<p align="center"><b>Adaptive Test-time Learning and Autonomous Specialization</b></p>

<p align="center">
  <img src="https://img.shields.io/badge/version-V3.1.7-blue" alt="Version"/>
  <img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="License"/>
  <img src="https://img.shields.io/badge/model-agnostic-green" alt="Model-agnostic"/>
</p>

<p align="center">
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/test.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/test.yml?branch=main&label=tests" alt="Tests"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/install-test.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/install-test.yml?branch=main&label=install%20matrix" alt="Install matrix"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/codeql.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/codeql.yml?branch=main&label=codeql" alt="CodeQL"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/container-scan.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/container-scan.yml?label=container%20scan" alt="Container scan"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/verify-tags.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/verify-tags.yml?label=release%20signature" alt="Release signature"/></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/inferstep/ATLAS"><img src="https://api.scorecard.dev/projects/github.com/inferstep/ATLAS/badge" alt="OpenSSF Scorecard"/></a>
</p>


## 🌎 ATLAS とは

**ATLAS は、フロンティアモデル級の推論と検証をコンパクトなオープンモデルにもたらす、ローカルで動くコーディングエージェントです。** モデルを取り巻くシステム側（プランニング、候補生成、品質スコアリング、サンドボックスでのテスト、修復）により多くの知性を持たせることで、小さなモデルでも実際のソフトウェア開発の仕事を、ホスト型 API にもトークン課金にも頼らず、すべて自分のハードウェア上でこなせるようにします。

## 💡 なぜ ATLAS なのか

* **小さなモデルからより多くを引き出す。** ATLAS は単発の生成に依存する代わりに、プランニング、候補選択、検証、修復をモデルの周りに重ねます。
* **受け入れる前に検証する。** 生成されたコードは、分離された実行環境の中でコンパイル・テスト・修正できます。
* **計算資源を要所に集中させる。** 単純な編集は短いパスで済ませ、難しいタスクにはより多くの候補・推論・検証を割り当てます。
* **自分のモデルを動かす。** NVIDIA、AMD、Apple Silicon、Vulkan、あるいは CPU 対応ハードウェア上で、互換性のある GGUF モデルを使えます。モデルにはレンズバンドルが必要で、ないモデルには `atlas lens build` で作成できます。
* **ローカルで管理する。** ATLAS は、リポジトリやプロンプトをホスト型モデルまたは ATLAS 運営サービスへ意図的にアップロードしません。サンドボックスコマンドはデフォルトで外部ネットワークへアクセスできます。無効にするには `ATLAS_SANDBOX_NET_INTERNAL=true` を設定してください。
* **スタック全体を所有する。** ATLAS はオープンソースかつセルフホストです。ホスト型モデルやサードパーティのモデルプロバイダー API キーは不要で、ローカルのインストール単位のサービストークンが ATLAS サービス間を認証します。

---

## 📰 最新リリース

現在のリリースは **[V3.1.7 "Maia"](https://github.com/inferstep/ATLAS/releases/tag/v3.1.7)** です。リリースノートはそのリリースページと [CHANGELOG](../../../CHANGELOG.md) にあります。

## ⭐ Star History

<!-- Self-hosted chart: rendered weekly by .github/workflows/star-chart.yml
     onto the `star-history` asset branch (scripts/star-history-chart.py).
     Replaces the star-history.com embed, whose shared token pool
     rate-limits unpredictably. -->
<a href="https://github.com/inferstep/ATLAS/stargazers">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/inferstep/ATLAS/star-history/star-history-dark.svg" />
   <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/inferstep/ATLAS/star-history/star-history-light.svg" />
   <img alt="Star history chart" src="https://raw.githubusercontent.com/inferstep/ATLAS/star-history/star-history-light.svg" width="100%" />
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
   - [予算ティア](../../ARCHITECTURE.md#pipeline-flow) - アロケータがタスクごとに計算ティアを選ぶ。ティアは候補数と、候補ごとのプロンプトおよびトークンの予算を決めます
   - [PR-CoT Repair](../../ARCHITECTURE.md#pipeline-flow) - サンドボックスのエラー出力をもとに失敗した候補を修復
   - [Refinement Loops](../../ARCHITECTURE.md#pipeline-flow) - サンドボックスでの検証と修正を繰り返す

4. **[Geometric Lens](../../ARCHITECTURE.md#5-geometric-lens)** - モデル自身の埋め込み上で動くエネルギーベースのスコアリング。外部オラクル不要。(「[Geometric Lens とは?](../../ARCHITECTURE.md#why-geometric-lens)」)
   - [C(x) Cost Field](../../ARCHITECTURE.md#scoring-models) - 候補の品質をスコア化する、モデルの隠れ次元→512→128→1 の MLP
   - [G(x) Quality Prediction](../../ARCHITECTURE.md#scoring-models) - 生成する候補数の判断に用いる XGBoost アンサンブル。候補の選択自体は最小の C(x) で行います
   - [パーステップスコアリング](../../API.md#geometric-lens-port-8099) - 書き込みのトークンごとの C(x)/G(x) スコアリング。モデル別の閾値が介入を動かすのは、そのモデルのバンドルが較正済みのときだけです

5. **[Sandbox](../../ARCHITECTURE.md#6-sandbox)** - ビルド検証のための分離実行環境。
   - 多言語実行: Python、Rust、Go、C、Shell など。Rust、Go、C はプロジェクト内のコマンドとして実行できます。V3 は構文を確認し、サンドボックスの `/execute` はコンパイルのみで、まだ実行できません ([#163](https://github.com/inferstep/ATLAS/issues/163))
   - スコアリング前のコンパイルとリント
   - 生成テストと既存テストスイートの両方を実行

6. **[llama-server](../../CONFIGURATION.md#6-llama-server)** - 単一のコンシューマ GPU 上でのローカル LLM 推論。
   - GPU 加速の量子化推論 (Q6_K / Q4_K_M) — NVIDIA CUDA、AMD ROCm、Apple Metal (macOS ハイブリッド)、Vulkan に対応。Intel SYCL はロードマップ
   - トークンレベルの文法制約デコーディング
   - セルフ埋め込み（レンズのための別モデルは不要）

詳細ドキュメント（セットアップ、アーキテクチャ、設定、トラブルシューティング、各コンポーネントの[研究的背景](../../SOURCES.md)）は [docs/](../../) にあります。

---

## 🚀 はじめに

ワンショットインストール:
```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

変化し続けるスクリプトをそのまま bash にパイプしたくない場合は、同じインストーラーをより慎重に実行する方法が2つあります:
```bash
# Pinned to a release: script, checkout, and images all at the signed tag
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/v3.1.7/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.7 bash

# Review before running
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

スクリプトはディストロ (Ubuntu、Debian、RHEL、Fedora、Rocky、Alma) と GPU ベンダー (NVIDIA → nvidia-container-toolkit; AMD → ROCm デバイスパススルー) を判定し、適切なランタイムをインストールし、モデル重みをダウンロードし、ASA ステアリングベクトルをビルドしてスタックを起動します。所要時間は 10〜30 分程度で、ボトルネックはモデルのダウンロードです。

完了後、任意のプロジェクトディレクトリで `atlas` を実行してください。

**要件**

| | |
|---|---|
| GPU | VRAM 16GB 以上。NVIDIA (CUDA、サポート対象 (Supported))、AMD (ROCm、コミュニティ検証済み (Community-tested))、または Apple Silicon (Metal、macOS ハイブリッド、サポート対象)。その他大半の GPU は Vulkan (プレビュー (Preview)) でカバー。プレビルドの CUDA イメージは Blackwell (RTX 50xx) を対象としており、それより古い NVIDIA GPU は一度だけのローカル再ビルドが必要です ([SETUP.md § CUDA Compute Capability](../../SETUP.md#cuda-compute-capability-dockerfilev31) を参照)。レベルの定義: [SUPPORT_MATRIX.md](../../SUPPORT_MATRIX.md)。GPU 一覧: [SETUP.md § Supported GPUs](../../SETUP.md#supported-gpus)。特定のモデルがお使いのカードに収まるかは [What fits on my GPU?](../../TROUBLESHOOTING.md#what-fits-on-my-gpu) を参照。 |
| ランタイム | Docker (NVIDIA: + nvidia-container-toolkit; AMD: 単体の Docker で十分) |
| Python | 3.9 以上 |
| ディスク | 約 20GB CUDA / 約 22GB ROCm (モデル重み + コンテナイメージ) |

Apple Silicon は macOS ハイブリッド Metal パス（ネイティブ llama-server + 残りは Docker — **[SETUP_MACOS.md](../../SETUP_MACOS.md)** を参照）でネイティブ動作します。Intel Arc (SYCL) はロードマップ上の項目です。手動インストール手順 (Docker Compose、ベアメタル、K3s) とブートストラップフラグの一覧は **[SETUP.md](./SETUP.md)** をご参照ください。

---

## ⚠️ 既知の制限事項

- **Linux の Docker スタック、加えてネイティブ macOS パス。** NVIDIA (サポート対象 (Supported))、AMD ROCm (コミュニティ検証済み (Community-tested))、Vulkan (プレビュー (Preview)) の Docker パスが現在存在します。Apple Silicon (サポート対象) はネイティブ macOS ハイブリッド Metal パス ([#32](https://github.com/inferstep/ATLAS/issues/32)) で動作します。Intel Arc / SYCL はロードマップ (Roadmap) です。レベルの定義: [SUPPORT_MATRIX.md](../../SUPPORT_MATRIX.md)。
- **ATLAS には現在のベンチマーク結果がありません。** V3.0 の LiveCodeBench の数値は撤回しました（[CHANGELOG](../../../CHANGELOG.md#withdrawn-the-v30-livecodebench-result-746) 参照）。現行リリースの能力・信頼性の数値はまだ測定されていません。数値は保留タスクでの再測定後にのみ公表し、モデル別の結果は [#28](https://github.com/inferstep/ATLAS/issues/28) で追跡します。
- **レンズのシグナルはまだ実証されていません。** リファレンスモデルではスコアは出ますが未較正のままで、介入は中立です。3.1.6 では有効なシグナルが得られず、`dev` ではまだ測定していません ([#281](https://github.com/inferstep/ATLAS/issues/281))。
- **複雑な機能追加は不安定なことがあります。** コンパクトなモデルは、コードを書き始める前に不慣れなコードベースの探索にエージェントターンを費やすことがあります。
- **文法制約デコーディングは制約なしのデコーディングより遅くなります。**

---

## 🗺️ ロードマップ

**3.2.0** ([マイルストーン](https://github.com/inferstep/ATLAS/milestone/2)) - 「完了」が本当に完了を意味するようにし、`dev` の内容をリリースします。
- 検証: 「完了」と実際に検証された内容の差を埋め ([#330](https://github.com/inferstep/ATLAS/issues/330))、ガードによる拒否を失敗として数えます ([#327](https://github.com/inferstep/ATLAS/issues/327))。
- 安全性: サンドボックスと初回インストールを強化し ([#384](https://github.com/inferstep/ATLAS/issues/384)、[#410](https://github.com/inferstep/ATLAS/issues/410)、[#483](https://github.com/inferstep/ATLAS/issues/483))、サービストークンをエンドツーエンドでテストします ([#212](https://github.com/inferstep/ATLAS/issues/212))。K3s も対象です ([#697](https://github.com/inferstep/ATLAS/issues/697))。
- IDE: VS Code 拡張機能をプレビューとして提供します。
- リリース: リリース候補を 3 日以上テストし ([#196](https://github.com/inferstep/ATLAS/issues/196))、署名とビルド証明を付けます ([#252](https://github.com/inferstep/ATLAS/issues/252)、[#361](https://github.com/inferstep/ATLAS/issues/361))。

**バックログ / 協力者募集**
- ハードウェア: ARM64 マルチアーキテクチャビルド ([#115](https://github.com/inferstep/ATLAS/issues/115))、大規模モデル向けのマルチ GPU ([#34](https://github.com/inferstep/ATLAS/issues/34))、Intel oneAPI / SYCL ([#27](https://github.com/inferstep/ATLAS/issues/27))。
- ツール: JetBrains プラグイン ([#35](https://github.com/inferstep/ATLAS/issues/35))。
- デプロイ: K3s / Kubernetes 上の ROCm。
- 研究: サンプリングを用いた推論 ([#9](https://github.com/inferstep/ATLAS/issues/9))。LiveCodeBench を隠しテスト付きで再実行 ([#28](https://github.com/inferstep/ATLAS/issues/28))。
- モデル: 自動化された HuggingFace 投稿 ([#102](https://github.com/inferstep/ATLAS/issues/102))。モデル非依存のバックエンド ([#66](https://github.com/inferstep/ATLAS/issues/66))。

---

## ❤️ ATLAS を支援する

ATLAS は、一人の大学生が自由時間に、1枚のコンシューマ GPU の上で開発しています。このプロジェクトが役に立っていて、持続可能な形で続いてほしいと思っていただけたら、ぜひ **[GitHub でのスポンサー](https://github.com/sponsors/itigges22)** をご検討ください。

スポンサーシップは以下に直接充てられます:

- **計算資源とハードウェア** - ベンチマークの反復を速めるための GPU 追加、メンテナーが自費では手の届かないアーキテクチャ（AMD ROCm、より大容量の VRAM カード、大規模モデル実験のためのクラウドレンタル）へのアクセス。
- **コントリビューターへの報奨金** - 実のある PR に本気で時間を割いてくれる外部コントリビューターへの相応の報酬。ATLAS が一人分のペースを超えて成長できるように。
- **研究** - このアーキテクチャを巡る継続的な学術活動。今後のワークショップ・学会への投稿から、アプローチを検証し拡張する論文執筆や共同研究まで。
- **コミュニティ** - ATLAS が動作するコミュニティとプラットフォームへの継続的なサポート。ドキュメント、ユーザー向けチャンネル、より多くの開発者に ATLAS を届け、既存ユーザーにより良く役立つための教育コンテンツを含みます。

すべてのスポンサーは、支援いただいたバージョンのリリースノートにクレジットされます。

---

## 🤝 コントリビュート

ATLAS はオープンに開発されており、コントリビューターとコアメンテナーを歓迎します。バグ修正、アクセラレータサポート、より大きなサブシステムの作業、いずれも歓迎です。

バグを見つけた、あるいは行き詰まった? **[Issue を作成してください](https://github.com/inferstep/ATLAS/issues)** — 修正の提出は必須ではありません。バグ報告とフィードバックはコードと同じくらい役に立ちます。

ガイドラインは **[CONTRIBUTING.md](../../../CONTRIBUTING.md)** を、コードベースのレイアウトの概要は[リポジトリマップ](../../README.md#repository-map)をご覧ください。

---

## 📄 ライセンス

[GNU Affero General Public License v3.0 (AGPL-3.0)](../../../LICENSE) の下でライセンスされています。
