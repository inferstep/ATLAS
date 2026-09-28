<!-- source: docs/SETUP.md synced-through: 4f1be83 -->
> **[English](../../SETUP.md)** | **[简体中文](../zh-CN/SETUP.md)** | **日本語** | **[한국어](../ko/SETUP.md)**

# ATLAS セットアップガイド

4 つのデプロイ方法があります: **ワンショットブートストラップ**（新規インストールに推奨）、Docker Compose（手動）、ベアメタル、K3s。

---

## インストールパスの選択

インストール手順はハードウェアと OS に依存します。お使いの構成に合う行を探して、リンク先のセクションに進んでください。

| ハードウェア | OS | 推奨パス | サポートレベル ([マトリクス](../../../SUPPORT_MATRIX.md)) |
|---|---|---|---|
| NVIDIA RTX 50 シリーズ / Blackwell (B100、GB10) | Linux | [方法 0: ブートストラップ](#方法-0-ワンショットブートストラップ) または [方法 1: Docker](#方法-1-docker-compose-推奨) | サポート対象 (Supported) — 公開されている CUDA イメージは Blackwell を対象 |
| NVIDIA RTX 20/30/40、GTX 10xx、データセンター (V100/A100/H100/T4/L4) | Linux | [方法 1: Docker](#方法-1-docker-compose-推奨) + 一度だけの[ローカル再ビルド](#cuda-compute-capability-dockerfilev31) | プレビュー (Preview) — ローカル再ビルドが必要 |
| NVIDIA GPU | Windows (WSL2) | [方法 1: Docker — NVIDIA セクション](#方法-1-docker-compose-推奨) | サポート対象外 (Unsupported) — 未テスト、動作の主張はなし。報告歓迎 |
| AMD GPU (RX 6000/7000、MI200+) | Linux | [方法 1: Docker — AMD ROCm](#amd-rocm--相違点) | コミュニティ検証済み (Community-tested) ([GH #26](https://github.com/itigges22/ATLAS/issues/26)) |
| **Apple Silicon (M1/M2/M3/M4)** | **macOS** | **[SETUP_MACOS.md](../../SETUP_MACOS.md)**（専用ガイド — ハイブリッドのネイティブ Metal + Docker） | サポート対象（メンテナー検証済み、M2 Pro） |
| Intel Arc / Iris Xe | Linux | [方法 1: Docker — Vulkan](#vulkan--クロスベンダーフォールバック) | プレビュー — Vulkan は lavapipe 上でのスモークテストのみ。実 GPU での検証はまだなし |
| Snapdragon X Elite（ラップトップ） | Linux | [Vulkan](#vulkan--クロスベンダーフォールバック) + [arm64 セクション](#arm64) | プレビュー（Linux arm64）。Windows on ARM はサポート対象外 |
| 古い AMD GPU (Vega、RDNA1、ROCm 6.x 非対応) | Linux | [方法 1: Docker — Vulkan](#vulkan--クロスベンダーフォールバック) | プレビュー |
| ARM64 上の NVIDIA (DGX Spark、Jetson) | Linux | [arm64 セクション](#arm64)（sbsa/l4t ベース差し替えによる CUDA） | プレビュー — ビルドレシピは提供済み。エンドツーエンドで検証されたデバイスはまだなし (#115) |
| Raspberry Pi 5 | Linux | [Vulkan + arm64](#arm64) | プレビュー — CPU 級の性能を想定 |
| Intel Mac（2020 年以前） | macOS | [方法 1: Docker — Vulkan](#vulkan--クロスベンダーフォールバック) | サポート対象外 — Docker Desktop が必要（未テスト）。Metal は Apple Silicon 専用 |
| CPU のみ、GPU なし | 任意 | [CPU のみのインストール](#cpu-only) | プレビュー — スモークテスト専用、非常に低速 |
| Kubernetes クラスター | Linux | [方法 3: K3s](#方法-3-k3s) | プレビュー — テンプレートは CI で検証済み。ライブクラスターでの自動テストはなし |
| ベアメタル / 開発用途 | Linux | [方法 2: ベアメタル](#方法-2-ベアメタル) | プレビュー — 手動検証のみ |

該当する構成が見つからない場合は、`uname -a` の出力と `lspci | grep -i vga`（Linux）/ `system_profiler SPDisplaysDataType`（Mac）を添えて Issue を作成してください。行を追加します。

---

## 方法 0: ワンショットブートストラップ

1 つの curl コマンドで、ディストロを検出し、Docker + nvidia-container-toolkit をインストールし、モデル重みをダウンロードし、スタックを立ち上げます。冪等なので再実行しても安全です。

> **NVIDIA の Blackwell 以前の GPU（RTX 20/30/40 シリーズ、GTX 10xx、V100/A100/T4/L4/H100）をお使いの方: まずこれをお読みください。**
> 公開されている `atlas-llama` CUDA イメージは compute capability
> `120;121`（Blackwell — RTX 50xx、B100、GB10）**のみ**を対象にコンパイルされています。それより古い NVIDIA GPU では
> llama-server が起動時に
> `no kernel image is available for execution on the device` で失敗します。
> 推論イメージをお使いの GPU のアーキテクチャ向けに一度だけ再ビルドしてください:
>
> ```bash
> # find your arch (drop the dot: 8.6 -> 86)
> nvidia-smi --query-gpu=compute_cap --format=csv,noheader
> docker compose build --build-arg CUDA_ARCH=86 llama-server   # example: RTX 30xx
> docker compose up -d --no-deps llama-server
> ```
> アーキテクチャの完全な対応表: [CUDA Compute Capability](#cuda-compute-capability-dockerfilev31)。

```bash
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

またはチェックアウト済みのリポジトリから:
```bash
./scripts/atlas-bootstrap.sh
```

**対応ディストリビューション:**

| ファミリー | ディストロ |
|---|---|
| Debian (apt-get) | Ubuntu 20.04+、Debian 11+ |
| RHEL (dnf) | RHEL 9+、Rocky 9+、AlmaLinux 9+、CentOS Stream 9+、Oracle Linux 9+ |
| Fedora (dnf) | Fedora 38+ |

`ID_LIKE` が上記のいずれかに一致するその他のディストロ（例: Linux Mint、Pop!_OS）は警告付きで受け付けます。このリストにないディストロ — Arch、openSUSE、Alpine、NixOS — はテストされておらず、スクリプトは実行を拒否します。

ブートストラップは、EPEL、nouveau ドライバの競合、libnvidia-ml.so.1 が見つからないケース（RHEL のミニマルインストール）、そして「docker グループにユーザーを追加したが現在のシェルにはまだ反映されていない」レースを回避します。

**モデル選択:** `.env.example` はモデル未選択の状態で出荷されます。ブートストラップが `.env` を作成した時点で `ATLAS_MODEL_FILE` が空の場合、レジストリのデフォルト推奨モデルを `.env` に書き込む（実行時にログに出ます）ため、ワンショットのフローはウィザードなしで完了します。選択は `.env` の編集または `atlas init` の実行でいつでも変更できます。既存の空でない選択は尊重されます。

<a id="cpu-only"></a>
**CPU のみ / GPU なしのホスト（プレビュー (Preview) — スモークテスト専用）。** ATLAS は Vulkan イメージの lavapipe CPU ラスタライザ経由で GPU なしでも起動しますが、推論は非常に低速です。スタックが動くことの確認に使い、実際のコーディングセッションには使わないでください。

1. **ブートストラップは、明示的にオプトインしない限り GPU なしのホストを拒否します:**
   `ATLAS_BOOTSTRAP_SKIP_GPU=1 ./scripts/atlas-bootstrap.sh`
   これは `docker-compose.vulkan.yml` を重ね（`/dev/dri` がない場合はさらに `docker-compose.cpu.yml` も）、モデル選択と `ATLAS_BACKEND=vulkan|cpu` を自ら `.env` に書き込み、ASA ビルドをスキップします。
2. **GPU のないホストで `atlas init` を実行しないでください** — ウィザードは、サイズを決められない `.env` を書き込む代わりに、意図的に拒否します（exit 1）。モデル選択はブートストラップが処理します。モデルは後から `atlas model install` で変更してください。

手動での同等手順:

```bash
cp .env.example .env    # set ATLAS_MODEL_FILE / ATLAS_MODEL_NAME
atlas model install Qwen3.5-9B-Q6_K
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml -f docker-compose.cpu.yml up -d
atlas doctor            # gpu check WARNS ("CPU-only mode — very slow"); warns exit 0
```

**ファイアウォール:** Compose スタックはすべてのサービスを `127.0.0.1` にのみ公開するため、ローカル利用にファイアウォールの変更は不要で、ブートストラップはデフォルトで firewalld に触れません。サービスをルーティング可能なインターフェースに再バインドするデプロイでは、`ATLAS_BOOTSTRAP_OPEN_FIREWALL=1` を設定するとサービスポート（8090、8099、8070、30820）が開放されます。

**実行モード — どちらでも動作します:**

```bash
# Run as your normal user; sudo elevates as needed (Docker install, etc).
# Install ends up owned by you.
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh | bash

# Run via sudo. SUDO_USER is detected, install still ends up owned by you.
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh | sudo bash

# Real root login (no sudo) — install owned by root. Only do this if there's
# no human user on the box (CI runner, container, etc).
```

**慎重なインストールの選択肢**（同じスクリプトです。変化し続ける `main` のスクリプトをそのまま bash にパイプしたくない方向け）:

```bash
# Pinned to a release: fetch the script AT the tag and install that tag.
# The checkout is pinned to the (SSH-signed) tag and ATLAS_IMAGE_TAG is
# pinned to the matching cosign-signed images.
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/v3.1.3/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.3 bash

# Review before running: download, read, then execute the same bytes.
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

**設定用の環境変数:**

| フラグ | 効果 |
|---|---|
| `ATLAS_BOOTSTRAP_SKIP_DOCKER=1` | Docker をインストールしない（すでに管理済みの場合） |
| `ATLAS_BOOTSTRAP_SKIP_GPU=1` | GPU ランタイムのインストール（NVIDIA toolkit または ROCm セットアップ）をスキップ。 |
| `ATLAS_BOOTSTRAP_SKIP_MODELS=1` | モデル重みをダウンロードしない |
| `ATLAS_BOOTSTRAP_SKIP_COMPOSE=1` | `docker compose up` を実行しない |
| `ATLAS_BOOTSTRAP_SKIP_ASA=1` | ASA ステアリングベクトルのビルドをスキップ（デフォルト: サービス起動の約 5 分後にビルド。GPU がない場合は自動的にスキップ） |
| `ATLAS_BOOTSTRAP_OPEN_FIREWALL=1` | firewalld でサービスポートを開放（デフォルト: オフ — サービスはループバックにバインド） |
| `ATLAS_BOOTSTRAP_NO_SUDO=1` | sudo を試みる代わりに失敗する |
| `ATLAS_BOOTSTRAP_REF=vX.Y.Z` | `main` を追跡する代わりに git タグ/SHA にインストールを固定。`vX.Y.Z` 形式の値なら `ATLAS_IMAGE_TAG` も対応するイメージに固定されます |
| `ATLAS_INSTALL_DIR=/path` | クローン先（デフォルト `/opt/atlas` — 下記参照） |
| `ATLAS_REPO_URL=https://...` | 別のリポジトリ URL |
| `ATLAS_GO_VERSION=1.26.2` | TUI ビルド用にインストールされる Go ツールチェーンのバージョン（TUI には 1.26.2+ が必要。より古いインストール済みツールチェーンは自動で取得します） |

**なぜ `/opt/atlas` なのか?** システム全体で使うサードパーティソフトウェアの標準的な FHS プレフィックスであり、`$HOME` のクリーンアップを生き残り、同じマシンの複数ユーザーが 1 つのインストールを共有できるためです。ホームディレクトリに置きたい場合は:

```bash
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh \
  | ATLAS_INSTALL_DIR=$HOME/atlas bash
```

完了すると、クイックスタートコマンド付きの緑色の「ATLAS ready」バナーが表示されます。高速な回線を持つ新規 VM での合計時間: 約 10〜30 分（モデルダウンロードが支配的）。

各ステップを手動で行いたい場合は、下記の方法 1 を使ってください。

---

## 前提条件 (全方法共通)

| 要件 | 詳細 |
|-------------|---------|
| **GPU** | VRAM 16 GB 以上。NVIDIA (CUDA、サポート対象 (Supported) — 公開イメージは Blackwell を対象。より古いカードは一度だけの[ローカル再ビルド](#cuda-compute-capability-dockerfilev31)が必要)。AMD (ROCm、コミュニティ検証済み (Community-tested))。Apple Silicon (Metal、macOS ハイブリッド、サポート対象 — [SETUP_MACOS.md](../../SETUP_MACOS.md) を参照)。Vulkan (プレビュー (Preview)) はクロスベンダーのフォールバック。Intel Arc (SYCL) はロードマップ (Roadmap)。[§ 対応 GPU](#対応-gpu) を参照。 |
| **GPU ドライバ** | NVIDIA: プロプライエタリドライバ（`nvidia-smi` で GPU が表示されること）。AMD: `amdgpu-dkms` カーネルドライバ（`/dev/kfd` が存在すること。`rocm-smi` で GPU が表示されること）。 |
| **Python 3.9+** | pip 付き |
| **curl** | モデル重みのダウンロード用 |
| **モデル重み** | ホストに収まるレジストリモデルまたは持ち込みの GGUF。`atlas init` が推奨を提示し、選択を `.env` に書き込みます。 |

### GPU の確認

**NVIDIA:**

```bash
nvidia-smi
# Should show your GPU with driver version and VRAM
# If this fails, install NVIDIA proprietary drivers first
```

**AMD:**

```bash
rocm-smi --showproductname --showmeminfo vram
# Should show your GPU model and total VRAM
# If rocm-smi is missing or /dev/kfd doesn't exist, install ROCm:
#   RHEL 9: sudo dnf install -y https://repo.radeon.com/amdgpu-install/6.2/rhel/9.4/amdgpu-install-6.2.60200-1.el9.noarch.rpm
#           sudo amdgpu-install --usecase=dkms,rocm
#   Ubuntu: Follow https://rocm.docs.amd.com/projects/install-on-linux/
# Then REBOOT.
```

**自動検出** — `atlas tier` にベンダー横断で自動検出させ、何が見つかったかを報告させます:

```bash
pip install -e .
atlas tier              # prints detected GPU, tier classification, recommended settings
atlas tier --json       # machine-readable (used by atlas init wizard)
```

---

## 方法 1: Docker Compose (推奨)

もっとも入念に検証されているデプロイ方法です: CI は compose ファイルを検証し、コントロールプレーン全体を決定論的に駆動し（フェイク推論）、リリースは実ハードウェア上の Compose でスモークテストされます。実際の GPU 推論の挙動は、下のハードウェア表に記載のカードで検証されており、GitHub ホストの CI では検証されません。

### 追加の前提条件

**NVIDIA (CUDA):**
- **Docker**（[nvidia-container-toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html) 付き）、**または Podman**（同じ toolkit 付き）
- 約 20 GB のディスク容量（モデル重み + コンテナイメージ）

**AMD (ROCm):**
- **Docker** 単体 — ROCm に別個のコンテナランタイムは不要で、`--device=/dev/kfd --device=/dev/dri` によるパススルーで十分です
- ユーザーが `video` と `render` グループに属している必要があります: `sudo usermod -aG video,render $USER`（その後再ログイン）
- 約 22 GB のディスク容量（ROCm イメージは CUDA 相当より約 2 GB 大きい）

### セットアップ

```bash
# 1. Clone
git clone https://github.com/itigges22/ATLAS.git
cd ATLAS

# 2. Install the ATLAS CLI (puts `atlas` in ~/.local/bin)
pip install --user -e .

# Make sure ~/.local/bin is on your PATH so `atlas` resolves:
case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *)
   echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
   source ~/.bashrc
;; esac

# 3. Select/install a model and write model-aware runtime sizing
atlas init

# 4. Install Go 1.26.2+ — required for the TUI client (atlas tui) and
#    optional for the proxy (proxy builds automatically on first run if Go
#    is present; otherwise it runs in Docker with file access limited to
#    ATLAS_PROJECT_DIR). Quickest path:
mkdir -p /tmp/go-install && cd /tmp/go-install
curl -LO https://go.dev/dl/go1.26.2.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.2.linux-amd64.tar.gz
echo 'export PATH="/usr/local/go/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
cd -

# Then build the TUI:
cd tui && go build -o ~/.local/bin/atlas-tui . && cd ..

# 5. Review the environment generated by `atlas init`
#    ATLAS_MODEL_FILE and ATLAS_MODEL_NAME identify this installation's
#    selected model; they are intentionally not project-wide defaults.
${EDITOR:-vi} .env

# 6. Start all services (first run builds container images — this takes several minutes)
#    NVIDIA hosts (default):
docker compose up -d                                                  # or: podman-compose up -d
#    AMD ROCm hosts:
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
#    `atlas init` writes a marker comment into .env telling you which to use.

# 7. Verify everything is healthy (wait for all services to show "healthy")
docker compose ps

# 8. Start coding (from your project directory)
cd /path/to/your/project
atlas
```

#### AMD ROCm — 相違点

ROCm パスは、次の 3 点を*除いて* NVIDIA と同一です:

1. **両方の compose ファイルで立ち上げる**（または `atlas init` に任せる）:
   ```bash
   docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
   ```
   オーバーライドは llama-server のイメージを ROCm ビルドに切り替え、NVIDIA のドライバ要求を `/dev/kfd` + `/dev/dri` のパススルーに差し替え、エントリーポイントが HIP チューニング分岐を取るよう `ATLAS_BACKEND=rocm` を強制します。

2. **`nvidia-container-toolkit` は不要** — ROCm に別個のコンテナランタイムは要らず、カーネルレベルのデバイスアクセスだけで済みます。ユーザーが正しいグループに属していることを確認してください:
   ```bash
   id -nG | tr ' ' '\n' | grep -E '^(render|video)$'
   # Should print both. If not:
   sudo usermod -aG video,render $USER
   # Then log out + back in (or: newgrp render)
   ```

3. **GPU のコンピュートターゲット。** デフォルトの `Dockerfile.rocm` ビルドは、RDNA3（7000 シリーズ）、RDNA2（6000 シリーズ）、CDNA2（MI200）をカバーする「肥大」イメージです — `gfx1100;gfx1101;gfx1102;gfx1030;gfx90a`。特定の GPU に絞ったより小さいイメージを作るには、ビルド前に `ATLAS_GFX_TARGET` を設定してください:
   ```bash
   # Example: only build for RX 7900 XT/XTX
   ATLAS_GFX_TARGET=gfx1100 docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
   ```
   お使いのカードの gfx ターゲットは [LLVM AMDGPU プロセッサ表](https://llvm.org/docs/AMDGPUUsage.html) を参照してください。

「非対応の GPU だが ROCm がそれなりに動く」ケース（古い Vega、RDNA1）については、`ATLAS_HSA_OVERRIDE_GFX_VERSION` の回避策を [TROUBLESHOOTING.md § AMD GPU が検出されない](./TROUBLESHOOTING.md) で確認してください。

#### Vulkan — クロスベンダーフォールバック

ネイティブのベンダーバックエンドがお使いのハードウェア向けにパッケージされていない場合（Intel Arc、Snapdragon Adreno、ROCm 6.x 非対応の古い AMD）、Vulkan がフォールバックです。1 つの Dockerfile で AMD（Mesa RADV）、Intel（Mesa ANV）、NVIDIA（nvidia-container-toolkit）、Apple（macOS Docker 経由の MoltenVK）、Snapdragon（Adreno）、CPU（Mesa lavapipe）をカバーします。

トレードオフ: チューニング済みのネイティブバックエンドより通常 20〜40% 遅くなります。CUDA/ROCm が選択肢にないとき、あるいは珍しいハードウェアで ATLAS が起動するかをスモークテストするときに使ってください。

```bash
# Option A — let atlas init pick it for you
# (the wizard offers Vulkan when your GPU vendor's native backend isn't packaged,
#  or run with --backend vulkan to force it regardless of vendor):
atlas init --backend vulkan
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml up -d

# Option B — already-installed deployment, just switch the override file:
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml up -d
# (the entrypoint dispatches on ATLAS_BACKEND, which the compose overlay
#  sets to vulkan; .env's value is ignored when the overlay is in play)
```

CUDA/ROCm との相違点:

1. **ベンダー固有のカーネルドライバ要件がない。** Vulkan ICD はイメージ内に含まれます（`mesa-vulkan-drivers` が AMD/Intel/CPU をカバー。NVIDIA の ICD は nvidia-container-toolkit のマウント経由）。
2. **`/dev/dri` パススルーのみ** — `/dev/kfd` も `--gpus all` も不要（NVIDIA toolkit 経由でルーティングする場合を除く。その場合は両方とも引き続き動作します）。
3. **GPU の個別選択は `CUDA_VISIBLE_DEVICES` / `HIP_VISIBLE_DEVICES` の代わりに `ATLAS_VK_DEVICE_SELECT`。** フォーマットは Mesa 標準: `"vendorID:deviceID"`（16 進）またはデバイス名の部分文字列。`GGML_VK_VISIBLE_DEVICES`（数値インデックス）も動作します。
4. **`atlas doctor`** は `_check_vulkan_via_docker` プローブを実行します — ただし `ATLAS_BACKEND=vulkan` が設定されている場合のみ（それ以外では CUDA/ROCm の実行を速く保つためスキップします）。

GPU を期待していたのに `vulkaninfo` が `llvmpipe` の CPU デバイスしか表示しない場合は、カーネル側のデバイスパススルーが失敗しています — ホスト上に `/dev/dri/renderD*` が存在すること、ユーザーが `video` + `render` グループに属していること（上記の ROCm と同じ要件）を確認してください。

<a id="arm64"></a>
#### arm64 ホスト (#115)

ATLAS は 2 つの CPU アーキテクチャを対象としています: `x86_64`（デフォルト、全バックエンド利用可能）と `aarch64`（バックエンドの一部）。`atlas doctor` で確認できます — `arch` チェックが、GPU チェックの前に、アーキテクチャとそこで利用可能なバックエンドを表示します。

**アーキテクチャ別のバックエンド対応状況:**

| バックエンド | x86_64 | aarch64 | 備考 |
|---|---|---|---|
| CUDA | 対応（rockylinux9 ベース） | 対応（sbsa または l4t ベース、build-arg 差し替え） | DGX Spark = sbsa、Jetson = l4t |
| ROCm | 対応 | **非対応** | AMD に arm64 の ROCm リリースはありません。代わりに Vulkan を使ってください。 |
| Vulkan | 対応 | 対応（Mesa はマルチアーキテクチャ） | すべての arm64 GPU 向けのユニバーサルフォールバック |
| CPU (lavapipe) | 対応 | 対応 | 遅いが常に動作 |

**対象の arm64 デバイス:**

- **NVIDIA DGX Spark**（Grace-Blackwell GB10）— sbsa ベースイメージによる CUDA、compute cap 12.0/12.1
- **NVIDIA Jetson Orin / AGX / Nano** — l4t ベースイメージによる CUDA、compute cap 8.7
- **Apple Silicon (M1/M2/M3/M4)** — Docker Desktop の MoltenVK 経由の Vulkan（低速パス）。高速パスとしてのネイティブ Metal インストールは [#32](https://github.com/itigges22/ATLAS/issues/32) で追跡
- **Snapdragon X Elite**（Windows on ARM ラップトップ）— Adreno ドライバ経由の Vulkan
- **Raspberry Pi 5** — Mesa V3D ドライバ経由の Vulkan、CPU 級の性能を想定
- **Ampere Altra / AWS Graviton ワークステーション** — lavapipe 経由の Vulkan（コンシューマ向け arm64 dGPU がまだ存在しないため CPU フォールバック）

**arm64 向け Vulkan イメージのビルド:**

```bash
# Multi-arch build that produces a single image manifest covering both archs:
docker buildx build --platform linux/amd64,linux/arm64 \
  -t atlas-llama-server:vulkan \
  -f inference/Dockerfile.vulkan inference/
```

**arm64 向け CUDA イメージのビルド**（DGX Spark の例）:

```bash
# Swap to the sbsa-capable ubuntu base, build with --platform linux/arm64:
docker buildx build --platform linux/arm64 \
  --build-arg BUILDER_IMAGE=nvidia/cuda:12.9.0-devel-ubuntu22.04 \
  --build-arg RUNTIME_IMAGE=nvidia/cuda:12.9.0-runtime-ubuntu22.04 \
  -t atlas-llama-server:cuda-arm64 \
  -f inference/Dockerfile.v31 inference/
```

Jetson では、両方の build arg を `nvcr.io/nvidia/l4t-jetpack:r36.3.0` に差し替えてください（l4t は JetPack + CUDA + cuDNN を 1 つのイメージとして出荷しています）。

**既知のギャップ（#115 で追跡）:**

- GHCR にまだ arm64 のプレビルドイメージがない — arm64 ユーザーは上記のレシピでローカルビルドが必要です。少なくとも 1 つの arm64 デバイスがエンドツーエンドで検証され次第、プレビルドのマルチアーキイメージが提供されます。
- ブートストラップインストーラー（`scripts/atlas-bootstrap.sh`）は arm64 パスの監査が済んでいません。
- 5 つの対象デバイスすべてでハードウェアテストのマトリクスが空です — これらのいずれかをお持ちのアーリーアダプターの方は、`atlas doctor` の出力と `vulkaninfo --summary` を [#115](https://github.com/itigges22/ATLAS/issues/115) に投稿してください。

### 初回実行時の動作

1. Docker が `ghcr.io/itigges22/atlas-{proxy,v3,lens,llama,sandbox}` から 5 つのプレビルドコンテナイメージを取得します（高速な回線で約 3 分）。代わりにソースからビルドする場合（開発パス）は、`up` の前に `docker compose build` を実行してください — 下の「イメージソース」を参照。
2. llama-server が 7GB のモデルを GPU VRAM にロード（約 1〜2 分）
3. 全サービスがヘルスチェックを開始
4. 5 つのサービス（llama-server、geometric-lens、v3-service、sandbox、atlas-proxy）すべてが healthy を報告すると、`atlas` が接続して Bubbletea TUI を起動します

2 回目以降の `docker compose up -d` はイメージがキャッシュされているため高速（数秒）です。

### イメージソース: プレビルド vs ソースビルド

`docker-compose.yml` はすべてのサービスについて `image:`（GHCR）と `build:`（ローカル Dockerfile）の両方を宣言しています。Compose のデフォルトの挙動:

| コマンド | 動作 |
|---------|--------------|
| `docker compose up -d`            | `image:` がローカルキャッシュになければ取得、あれば再利用 |
| `docker compose pull`             | GHCR から最新タグを強制取得（ローカルキャッシュを上書き） |
| `docker compose build`            | `Dockerfile` からビルド（GHCR イメージを上書き） |
| `docker compose up -d --build`    | 常にソースから再ビルドしてから起動 |

**タグの固定。** タグのデフォルトは `latest` です。特定のバージョンに固定するには（本番では推奨）、`.env` で `ATLAS_IMAGE_TAG` を設定してください:

```env
ATLAS_IMAGE_TAG=3.1.3      # semver tag from a git release
ATLAS_IMAGE_TAG=sha-abc1234  # exact commit
ATLAS_IMAGE_TAG=dev          # bleeding edge from dev branch
```

利用可能なタグの一覧: <https://github.com/itigges22/ATLAS/pkgs/container/atlas-proxy>
（`atlas-proxy` を他のサービス名に置き換えてください: `atlas-v3`、`atlas-lens`、`atlas-llama`、`atlas-sandbox`）。

エッジケース: GHCR でまだ非公開のパッケージに対して `compose pull` は `unauthorized` で失敗します — `read:packages` トークンで認証するか、代わりにソースからビルドしてください。`compose pull` は同じタグを共有するローカルビルド済みイメージも上書きします。サービスを反復開発している間は pull をスキップするか、`ATLAS_IMAGE_TAG=dev-local` を設定してローカルとレジストリのイメージを別々のタグに分けてください。フォークのイメージを取得するには `.env` に `ATLAS_GHCR_OWNER=<your-username>` を設定します。

### インストールの確認

最速の方法は **`atlas doctor`** です — ホスト環境（GPU ランタイム、モデルと lens のアーティファクト）、docker スタック（コンテナ、ヘルスエンドポイント、認証、ステート）、そして実際のモデル推論をチェックし、完了ごとに結果を表示し、exit 0（正常）/ 1（失敗）を返します。チェックの正確な数は、バックエンド、スタックの状態、フラグによって変わります。`atlas-bootstrap.sh` がインストールの最後に実行するのもこれです。

```bash
atlas doctor              # full check (~5–10s)
atlas doctor --quick      # skip the e2e model inference (~2s)
atlas doctor --json       # machine output, for scripts/CI (buffered, one JSON document)
atlas doctor -v           # verbose: show detail for each check
```

チェックの一覧:

| グループ | チェック | 確認内容 |
|---|---|---|
| ホスト | docker | デーモンに到達可能 |
| ホスト | compose | docker compose v2 がインストール済み |
| ホスト | arch | CPU アーキテクチャ（`x86_64` / `aarch64`）とその上で利用可能なバックエンド (#115) — GPU チェックの前に常に実行 |
| ホスト | gpu | ベンダー対応の GPU ランタイム: NVIDIA（nvidia-container-toolkit が Docker 内で nvidia-smi を実行）または AMD（`/dev/kfd` パススルー）。GPU が検出されない場合は警告 |
| ホスト | vulkan | Docker 内から Vulkan ICD が見える — `ATLAS_BACKEND=vulkan` のときのみ |
| ホスト | metal-native | ネイティブ llama-server バイナリが存在し実行可能 — `ATLAS_BACKEND=metal`（macOS ハイブリッド）のときのみ |
| ホスト | model_file | `.env` で選択された `ATLAS_MODEL_FILE` が存在し 100 MB 超 |
| ホスト | lens_weights | `cost_field.pt` + G(x) アーティファクトが存在 |
| ホスト | asa_steering | `ast_edit_steering.gguf` が存在（BiasBusters #4 — fail ではなく warn。ATLAS はこれなしでも動作しますが、structural_edit と edit_file のバイアスがステアされないままになります） |
| ホスト | tier_match | `.env` のモデル選択がホストのハードウェアに合致（オーバーシュートは警告 — OOM リスク。一致またはアンダーシュートは pass） |
| ホスト | tier_constraints | ホストの CPU/RAM/ディスクが推奨ティアの最小値を満たす（「GPU は 16 GB だが RAM は 8 GB」の不一致を捕捉） |
| スタック | container/llama-server, geometric-lens, v3-service, sandbox, atlas-proxy | 5 つすべてが実行中かつ healthy |
| スタック | health/llama, lens, v3, sandbox, proxy | 5 つの `/health` エンドポイントすべてが ok を返す |
| スタック | internal_auth | 内部サービス認証: トークンファイルが厳格なパーミッションで存在し、実際の強制が双方向でプローブされる（誤ったトークン → 401、有効なトークンは受理）。認証が無効（`secrets/service-token` なし）の場合は警告 |
| スタック | status_dimensions | 情報提供のみ: プロキシの `/v1/calibration/status` から得られる 7 つの lens/ASA ステータスディメンション（TUI のバッジが読むのと同じソース）。この行が実行を失敗させることはない |
| スタック | image_skew | 5 つの `atlas-*` イメージすべてが同じタグ |
| エンドツーエンド | e2e_smoke | llama-server への実際の `/v1/chat/completions` ラウンドトリップ（`--quick` でスキップ） |

`vulkan` と `metal-native` の行は設定されたバックエンドに応じた条件付きです。health、`internal_auth`、`status_dimensions` の行は少なくとも 1 つのコンテナが起動している場合にのみ実行され、`e2e_smoke` は `--quick` でスキップされます。残りのチェックは常に実行されます。

手動で確認したい場合:

```bash
# Hit each health endpoint
curl -s http://localhost:8080/health | python3 -m json.tool   # llama-server
curl -s http://localhost:8099/health | python3 -m json.tool   # geometric-lens
curl -s http://localhost:8070/health | python3 -m json.tool   # v3-service
curl -s http://localhost:30820/health | python3 -m json.tool  # sandbox
curl -s http://localhost:8090/health | python3 -m json.tool   # atlas-proxy

# 機能テスト: インストール全体の診断（サービス、アーティファクト、e2e スモーク）
atlas doctor
```

すべてのヘルスエンドポイントが `{"status": "ok"}` または `{"status": "healthy"}` を返すはずです。

> **注意:** 対話端末での素の `atlas` は、完全なエージェントループ（ツールコール、V3 パイプライン、ファイル読み書き）のための Bubbletea TUI を起動します。TUI には実際の端末が必要です — stdin/stdout がパイプされている場合は `atlas doctor` への案内を表示して終了します。

### 停止

```bash
docker compose down          # Stop all services (preserves images)
docker compose down --rmi all  # Stop and remove images (next start rebuilds)
```

### ログの確認

```bash
docker compose logs -f llama-server    # Follow llama-server logs
docker compose logs -f geometric-lens  # Follow Lens logs
docker compose logs -f v3-service      # Follow V3 pipeline logs
docker compose logs -f atlas-proxy     # Follow proxy logs
docker compose logs -f sandbox         # Follow sandbox logs
docker compose logs --tail 50          # Last 50 lines from all services
```

### アップデート

```bash
git pull
docker compose down
docker compose pull          # grab fresh :latest images from GHCR
docker compose up -d
```

### アンインストール

```bash
# Stop and remove the containers, network, and named volumes
docker compose down -v

# Remove the published images
docker images "ghcr.io/*/atlas-*" -q | xargs -r docker rmi

# Remove the CLI and TUI binaries
pip uninstall atlas
rm -f ~/.local/bin/atlas-tui
rm -rf ~/.cache/atlas-tui          # TUI session history

# The repo checkout, .env, and downloaded models live wherever you put
# them — delete the checkout and its models/ directory to reclaim the
# disk (models are the multi-GB part).
```

K3s のインストールでは代わりに `scripts/uninstall.sh` を使います。マニフェストを削除し、（任意で）K3s ノード自体も削除します。

---

## 方法 2: ベアメタル

コンテナを使わず、すべてのサービスをローカルプロセスとして実行します。開発用途や Docker が利用できないシステムに適しています。

### 追加の前提条件

| 要件 | 詳細 |
|-------------|---------|
| **Go 1.26.2+** | atlas-proxy と atlas-tui クライアントのビルド用（より古い Go ツールチェーンは自動で取得します） |
| **llama.cpp** | CUDA 付きでソースからビルド（[llama.cpp ビルド手順](https://github.com/ggml-org/llama.cpp?tab=readme-ov-file#build) を参照） |
| **Node.js 20+** | サンドボックスの JavaScript/TypeScript 実行に必要 |
| **Rust** | サンドボックスの Rust 実行に必要 |

### ビルド

```bash
# 1. Clone and install Python CLI
git clone https://github.com/itigges22/ATLAS.git
cd ATLAS
pip install -e .

# 2. Select and install a registry model (or place a BYO GGUF in models/)
atlas model recommend
atlas model install <registry-name>

# 3. Build the proxy
cd proxy
go build -o ~/.local/bin/atlas-proxy-v2 .
cd ..

# 4. Install geometric-lens Python dependencies
pip install -r geometric-lens/requirements.txt

# 5. Install V3 service PyTorch (CPU only)
pip install torch --index-url https://download.pytorch.org/whl/cpu

# 6. Install sandbox dependencies
pip install -r sandbox/requirements-runtime.txt -r sandbox/requirements-verify.txt
```

### サービスの起動

各サービスを別々のターミナルで起動します（または `&` を使ってログファイルにリダイレクトします）:

```bash
# Terminal 1: llama-server (GPU)
llama-server \
  --model "models/$ATLAS_MODEL_FILE" \
  --host 0.0.0.0 --port 8080 \
  --ctx-size 32768 --n-gpu-layers 99 --no-mmap \
  --embeddings --pooling mean --flash-attn on --fit off

# Terminal 2: Geometric Lens
cd geometric-lens
LLAMA_URL=http://localhost:8080 \
LLAMA_EMBED_URL=http://localhost:8080 \
GEOMETRIC_LENS_ENABLED=true \
PROJECT_DATA_DIR=/tmp/atlas-projects \
python -m uvicorn main:app --host 0.0.0.0 --port 8099

# Terminal 3: V3 Pipeline
cd v3-service
ATLAS_INFERENCE_URL=http://localhost:8080 \
ATLAS_LENS_URL=http://localhost:8099 \
ATLAS_SANDBOX_URL=http://localhost:8020 \
python main.py

# Terminal 4: Sandbox
cd sandbox
python executor_server.py

# Terminal 5: atlas-proxy
ATLAS_PROXY_PORT=8090 \
ATLAS_INFERENCE_URL=http://localhost:8080 \
ATLAS_LLAMA_URL=http://localhost:8080 \
ATLAS_LENS_URL=http://localhost:8099 \
ATLAS_SANDBOX_URL=http://localhost:8020 \
ATLAS_V3_URL=http://localhost:8070 \
ATLAS_MODEL_NAME="${ATLAS_MODEL_NAME:-local-model}" \
atlas-proxy-v2
```

> **注意:** サンドボックスはベアメタルモードではポート **8020** でリッスンします（Docker のポートリマッピングなし）。プロキシの `ATLAS_SANDBOX_URL` には 30820 ではなくポート 8020 を使用してください。

### TUI の起動

`atlas` コマンドは Python パッケージのコンソールエントリポイントで、ビルド手順の `pip install -e .` によってインストール済みです — 別途ランチャースクリプトは不要です。上記のサービスが起動している状態で:

```bash
cd /path/to/your/project
atlas    # Checks atlas-proxy is reachable, then launches the TUI
```

`atlas` は `atlas-tui` バイナリが見つからない、またはチェックアウトより古い場合に `tui/` から自動的にビルドし（PATH 上に Go 1.26.2+ が必要）、TUI に処理を渡す前に localhost:8090 のプロキシを検証します。

---

## 方法 3: K3s

GPU スケジューリング、ヘルスプローブ、リソース制限を備えた Kubernetes デプロイです。プレビュー (Preview) — テンプレートは CI で検証・レンダリングされますが、ライブクラスターでの自動テストはありません。

### 追加の前提条件

| 要件 | 詳細 |
|-------------|---------|
| **K3s** | シングルノードまたはマルチノードクラスター |
| **NVIDIA GPU Operator** または **device plugin** | GPU が `nvidia.com/gpu` リソースとして認識される必要があります |
| **Helm** | GPU Operator のインストール用 |
| **Podman または Docker** | コンテナイメージのビルド用 |

### 自動インストール

インストールスクリプトが完全なセットアップを処理します — K3s のインストール、GPU Operator、コンテナビルド、デプロイ:

```bash
# 1. Configure
cp atlas.conf.example atlas.conf
# Edit atlas.conf: model paths, GPU layers, context size, NodePorts

# 2. Run the installer (requires root)
sudo scripts/install.sh
```

インストーラーは以下を実行します:
1. 前提条件の確認（NVIDIA ドライバ、GPU VRAM、システム RAM）
2. K3s が未実行の場合はインストール
3. GPU がクラスターに認識されていない場合、Helm 経由で NVIDIA GPU Operator をインストール
4. コンテナイメージをビルドし、K3s containerd にインポート
5. `atlas.conf` から envsubst 経由でマニフェストを生成
6. `atlas` 名前空間にデプロイ
7. すべてのサービスが正常になるまで待機

### 手動デプロイ

K3s が既に GPU サポート付きで実行されている場合:

```bash
# 1. Configure
cp atlas.conf.example atlas.conf
# Edit atlas.conf

# 2. Build and import images
scripts/build-containers.sh

# 3. Generate manifests from atlas.conf
scripts/generate-manifests.sh

# 4. Deploy
kubectl apply -n atlas -f manifests/

# 5. Verify
scripts/verify-install.sh
```

### K3s 固有の設定

K3s は設定に `.env` ではなく `atlas.conf` を使用します。HTTP コントラクトとパイプラインの動作は Docker Compose と同一で、異なるのはデプロイの配管だけです:

| 設定項目 | Docker Compose | K3s |
|---------|---------------|-----|
| 設定ファイル | `.env` | `atlas.conf` |
| サービス公開 | ホストポート (`8090`, `8080`, `8099`, `8070`, `30820`) | NodePorts (`30080`, `32735`, `31144`, `30070`, `30820`) |
| プロジェクトワークスペース | バインドマウント (`ATLAS_PROJECT_DIR` → `/workspace`) | `hostPath`（`ATLAS_PROJECTS_DIR` → 必要な各 Pod の `/workspace`） |
| モデルファイル | バインドマウント (`ATLAS_MODELS_DIR` → `/models:ro`) | GPU ノード上の `hostPath`（`ATLAS_MODELS_DIR`、`Directory`、読み取り専用） |
| ステートフルストレージ | 名前付きボリューム (`v3-telemetry`) | PVC `lens-projects`（サイズは `ATLAS_PVC_PROJECTS_SIZE` で指定。マウントはされるが、プロジェクトインデクサの削除以降は未使用） |
| GPU 割り当て | `deploy.resources.reservations.devices` (nvidia) | `resources.limits.nvidia.com/gpu: 1`（GPU Operator またはデバイスプラグインが必要） |
| サンドボックスのツールチェーンキャッシュ | 言語ごとの `tmpfs` マウント | 言語ごとの `sizeLimit` 付き `emptyDir`（共通パターン、同一セット） |

モデル・ランタイムパラメータ（`ATLAS_MAIN_MODEL`、`ATLAS_CONTEXT_LENGTH`、`ATLAS_PARALLEL_SLOTS`、`ATLAS_FLASH_ATTENTION`、KV キャッシュ量子化、レンズスコアリング用の `--embeddings`）は、どちらのモードでも同じ環境変数から読み込まれます — `atlas.conf.example` と `.env.example` を参照してください。

全 `atlas.conf` リファレンスは [CONFIGURATION.md](../../CONFIGURATION.md) をご覧ください。

### K3s デプロイの確認

```bash
# Check pods
kubectl get pods -n atlas

# Check GPU allocation
kubectl describe nodes | grep nvidia.com/gpu

# Run verification suite
scripts/verify-install.sh
```

> **注意:** Docker Compose がもっとも入念に検証されているデプロイ方法です（CI が Compose に対して実行され、すべてのリリースが Compose でスモークテストされます）。K3s マニフェストは、デプロイ時に `scripts/generate-manifests.sh`（または `install.sh` の `process_templates` ステップ）経由で `templates/*.yaml.tmpl` から生成されます。テンプレートは `atlas.conf` で選択されたモデルを使用します。CHANGELOG のベンチマーク数値は、それぞれ固有の凍結されたモデル/構成を記録しています。

---

## ハードウェアサイジング

ATLAS は GPU を 5 つのティアに分類し、ティアごとにレジストリモデル + コンテキストサイズ + 並列スロットの構成を推奨します。これらは現時点のレジストリの推奨であり、ハードコードされたランタイム要件ではありません。`atlas tier` を実行すると、お使いのハードウェアがどのティアに該当するか、そして使うべき正確な `.env` 値が分かります。

| ティア | VRAM | 推奨モデル | コンテキスト | スロット | GPU の例 |
|------|------|-------------------|--------:|------:|--------------|
| **cpu** | n/a | [CPU のみのインストール](#cpu-only) — プレビュー、スモークテスト専用 | n/a | n/a | (GPU なし) |
| **small** | 8–12 GB | Qwen3.5 7B Q4_K_M (4.4 GB) | 8K | 1 | RTX 3060/4060 8GB, T4 |
| **medium** | 12–20 GB | Qwen3.5 9B Q6_K (6.9 GB) | 32K | 1 | RTX 4060/5060 Ti 16GB, 3080 Ti, 4070 Ti Super |
| **large** | 20–32 GB | Qwen3.5 14B Q5_K_M (10.5 GB) | 32K | 2 | RTX 3090, 4090, 5090 24GB |
| **xlarge** | 32 GB+ | Qwen3.5 32B Q5_K_M (23 GB) | 64K | 2 | RTX 5090 32GB, A6000, A100, H100 |

```bash
atlas tier              # classify this host + show recommendations
atlas tier list         # show all 5 tier definitions
atlas tier fit          # size the runtime for the CONFIGURED model + GPU
atlas tier --json       # machine output (for scripts)
atlas tier --raw        # just the probe (no classification)
```

ティア表は VRAM 帯ごとの出発点を与えるものです。**`atlas tier fit`** は、実際に動かす*特定の*モデルに合わせてそれを精密化します — GGUF の KV ジオメトリと GPU の VRAM を読み取り、完全に GPU 上に収まる最大のコンテキストを求めます（`atlas tier fit --write` で結果を `.env` に反映）。`ATLAS_MODEL_FILE` や GPU を変更するたびに実行してください。[CLI.md § atlas tier fit](../../CLI.md#atlas-tier-fit) と、ダウンロード前のサイジングの目安については [TROUBLESHOOTING.md § この GPU には何が収まる?](./TROUBLESHOOTING.md#この-gpu-には何が収まる) を参照してください。

medium ティアが ATLAS の開発ターゲットです — `atlas-bootstrap.sh` はそのモデル + コンテキスト設定をデフォルトにしています。他のティアでは、ブートストラップ完了後に**`atlas init`**（初回セットアップウィザード）を実行してください。`atlas tier` 経由でハードウェアをプローブし、レジストリから適切なモデルを選び、SHA 検証付きでダウンロードして `.env` を書き換えます。ハードウェアやレジストリのデフォルトモデルが変わったら `atlas init --reconfigure` を再実行してください。ウィザード実行後は `atlas tier fit --write` が、ウィザードのティアレベルのデフォルトを選択したモデル向けに引き締めます。

| リソース | 最小 | 推奨 | 備考 |
|----------|---------|-------------|-------|
| GPU VRAM | 8 GB | 16 GB | 上のティア表を参照 |
| システム RAM | 14 GB | 16 GB+ | PyTorch ランタイム + コンテナオーバーヘッド |
| ディスク | 15 GB | 25 GB | モデル（ティアにより 4.4–23 GB）+ コンテナイメージ（5–8 GB）+ 作業スペース |
| CPU | 4 コア | 8 コア以上 | V3 パイプラインは修復フェーズで CPU 負荷が高い |

### 対応 GPU

8 GB 以上の VRAM と llama.cpp 対応バックエンドを持つ任意の GPU:

| ベンダー | バックエンド | 状況 | ビルドパス | テスト済みカード |
|---|---|---|---|---|
| NVIDIA (Blackwell — RTX 50xx、B100、GB10) | CUDA | サポート対象 (Supported)（公開イメージ） | `inference/Dockerfile.v31` | RTX 5060 Ti 16GB（主要開発機） |
| NVIDIA (Blackwell 以前 — RTX 20xx–40xx、GTX 10xx、V100/A100/H100/T4/L4) | CUDA | プレビュー (Preview) — 一度だけの[ローカル再ビルドが必要](#cuda-compute-capability-dockerfilev31) | `inference/Dockerfile.v31` + `--build-arg CUDA_ARCH=<cc>` | —（上流の llama.cpp はこれらをサポート。ATLAS 上でのメンテナー検証はなし） |
| AMD | ROCm / HIP | コミュニティ検証済み (Community-tested) | `inference/Dockerfile.rocm` | RX 7900 XTX（コミュニティスモークテスト、[GH #26](https://github.com/itigges22/ATLAS/issues/26)） |
| Apple Silicon | Metal | サポート対象（macOS ハイブリッド: ネイティブ llama-server + Docker、[#32](https://github.com/itigges22/ATLAS/issues/32)） | `scripts/atlas-setup-macos.sh` + `docker-compose.macos.yml` | M2 Pro 32GB（検証済み）、M3/M4（対象） |
| 任意（クロスベンダーフォールバック） | Vulkan | プレビュー | `inference/Dockerfile.vulkan` | lavapipe（CPU ICD）でスモークテスト済み。実 GPU での検証はまだなし |
| Intel Arc | SYCL | ロードマップ (Roadmap) — Intel Arc は現在 Vulkan を使用 | 未定 | Arc A770 16GB（対象） |

`atlas tier` はベンダー横断で自動検出し、VRAM が最大の GPU を選択します。複数の GPU があり特定の 1 枚を使いたい場合は、`ATLAS_GPU_VENDOR=amd` や `ATLAS_GPU_INDEX=1` でオーバーライドしてください。

#### CUDA Compute Capability (Dockerfile.v31)

`inference/Dockerfile.v31` は llama.cpp を特定の CUDA compute capability 向けにコンパイルします。デフォルト — そして GHCR 上に公開されている `atlas-llama` イメージのビルドに使われている値 — は `120;121`（Blackwell: RTX 50xx、B100、GB10）**のみ**です。公開イメージにはそれより前の GPU 向けのカーネルが含まれず、埋め込まれた PTX を下位向けに JIT コンパイルすることもできないため、RTX 20/30/40 シリーズ、GTX 10xx、Blackwell 以前のデータセンターカード（V100/A100/H100/T4/L4）では、llama-server が起動時に `no kernel image is available for execution on the device` で失敗します。お使いのアーキテクチャ向けに、推論イメージを一度だけ再ビルドする必要があります。（誤った arch 値でのローカルビルドは、より早い段階で `nvcc fatal: unsupported gpu architecture` により失敗します。）

お使いの GPU の arch を確認し、`--build-arg CUDA_ARCH=<value>` で再ビルドしてください:

```bash
# Your GPU's compute capability (drop the dot: 8.9 -> 89)
nvidia-smi --query-gpu=compute_cap --format=csv,noheader

# Compose-native rebuild — only llama-server is rebuilt, the other
# services keep using the GHCR images (~30-75 min, one time):
docker compose build --build-arg CUDA_ARCH=89 llama-server
docker compose up -d --no-deps llama-server

# Or build the image directly:
podman build --build-arg CUDA_ARCH=89 -f inference/Dockerfile.v31 -t llama-server:local inference/

# Multiple archs (semicolon-separated) — build a fat binary for Ampere + Ada + Hopper
docker compose build --build-arg CUDA_ARCH="86;89;90" llama-server
```

一般的な値:

| Arch | アーキテクチャ | カード |
|------|--------------|-------|
| `60`, `61` | Pascal | GTX 10xx, Tesla P4/P40 |
| `70` | Volta | V100 |
| `75` | Turing | RTX 20xx, T4 |
| `80`, `86` | Ampere | A100, RTX 30xx |
| `89` | Ada Lovelace | RTX 40xx, L4 |
| `90` | Hopper | H100 |
| `100`, `120`, `121` | Blackwell | B100, RTX 50xx |

#### AMD GPU ターゲット (Dockerfile.rocm)

`inference/Dockerfile.rocm` は llama.cpp の HIP バックエンドを 1 つ以上の `gfx` ターゲット向けにコンパイルします。デフォルトは、もっとも一般的なコンシューマ + データセンター AMD GPU をカバーする肥大ビルドです: `gfx1100;gfx1101;gfx1102;gfx1030;gfx90a`。ターゲットを 1 つ追加するごとにバイナリが約 150 MB 増えます。

ビルド時に `--build-arg GFX_TARGET=<value>` でオーバーライドしてください（または compose オーバーライドが転送する `ATLAS_GFX_TARGET` 環境変数経由で）:

```bash
# Single target — RX 7900 XT/XTX only (smaller image)
ATLAS_GFX_TARGET=gfx1100 docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server

# Two targets for RDNA3 + RDNA2 mixed-fleet
docker build --build-arg GFX_TARGET="gfx1100;gfx1030" -f inference/Dockerfile.rocm -t atlas-llama-rocm:custom inference/
```

一般的な値:

| ターゲット | アーキテクチャ | カード |
|--------|--------------|-------|
| `gfx1100` | RDNA3 (Navi 31) | RX 7900 XT, 7900 XTX, 7900 GRE |
| `gfx1101` | RDNA3 (Navi 32) | RX 7800 XT, 7700 XT |
| `gfx1102` | RDNA3 (Navi 33) | RX 7600, 7600 XT |
| `gfx1030` | RDNA2 (Navi 21) | RX 6800, 6800 XT, 6900 XT, 6950 XT |
| `gfx1031` | RDNA2 (Navi 22) | RX 6700 XT, 6750 XT |
| `gfx1032` | RDNA2 (Navi 23) | RX 6600, 6600 XT, 6650 XT |
| `gfx90a` | CDNA2 | MI210, MI250, MI250X |
| `gfx942` | CDNA3 | MI300X |
| `gfx900` | Vega | Vega 56/64（HSA オーバーライドが必要な場合あり — TROUBLESHOOTING.md を参照） |
| `gfx1200` | RDNA4 (Navi 44) | RX 9070 |
| `gfx1201` | RDNA4 (Navi 48) | RX 9070 XT |

> **RDNA4 (gfx1200/gfx1201) ユーザーへ:** `ATLAS_ROCM_TAG=7.2.3-complete` を設定してください — デフォルトの ROCm 6.2 ベースイメージには gfx1200/gfx1201 のコンパイラサポートが含まれていません。ROCm 7.0+ はこれらのターゲットをネイティブにサポートします。`ATLAS_HSA_OVERRIDE_GFX_VERSION` は設定しないでください。詳細は [TROUBLESHOOTING.md § RDNA4](./TROUBLESHOOTING.md) を参照。

お使いの GPU の gfx ターゲットの確認: `rocminfo | grep -i gfx | head -1`（または [LLVM AMDGPU プロセッサ表](https://llvm.org/docs/AMDGPUUsage.html) で検索）。

---

## Geometric Lens ウェイト (オプション)

ATLAS は Geometric Lens ウェイトなしでも動作します — サービスはグレースフルにデグレードし、ニュートラルスコアを返します。V3 パイプラインはサンドボックスのみの検証にフォールバックします。

C(x)/G(x) スコアリングを有効にするには、トレーニング済みのモデルウェイトが必要です。事前トレーニング済みウェイトとトレーニングデータは HuggingFace で入手できます:

**[ATLAS Dataset on HuggingFace](https://huggingface.co/datasets/itigges22/ATLAS)** — エンベディング、トレーニングデータ、ウェイトファイルが含まれています。

ウェイトファイルを `geometric-lens/geometric_lens/models/` に配置してください（または Docker Compose で `ATLAS_LENS_MODELS` 経由でマウント）。サービスは起動時に自動的にロードします。

独自のベンチマークデータでトレーニングしたい場合、ループ全体が CLI で完結します:

```bash
atlas bench --run-id mymodel_lens --tasks 200    # 候補の生成とセルフラベリング
atlas lens build --force --from-results benchmark/results/mymodel_lens/v3_lcb/per_task
```

`atlas lens build` はレンズの両半分をトレーニングし、しきい値をキャリブレーションして、有効化されたバンドルに `provenance.json` マニフェストを書き出します。[CLI.md § atlas lens](../../CLI.md#atlas-lens) を参照してください。

### モデルの持ち込み

デフォルト以外の GGUF に差し替えたい場合、`atlas lens` サブコマンドがプローブ + トレーニングのパイプラインをラップするので、基盤のスクリプトを学ぶ必要はありません:

```bash
# 1. Drop your GGUF in models/ and update .env to point at it, restart llama-server.

# 2. Probe whether the existing artifacts can score it (cheap, no training):
atlas lens check
# Reports: compat (artifacts work) | needs-build (different dim) | incompatible

# 3. If 'needs-build', train fresh artifacts at the model's native embedding dim:
atlas lens build --samples path/to/labeled.json
# samples format: [{"text": str, "label": 0|1}, ...] where 1 = passing code
# Canonical training set: huggingface.co/datasets/itigges22/ATLAS

# 4. Re-run check — should now report compat:
atlas lens check
```

完全なリファレンス: [CLI.md § atlas lens](../../CLI.md#atlas-lens)。

---

## ASA ステアリングベクトル (自動ビルド)

2026年5月の BiasBusters #4。関数 / クラス / 要素全体の書き換えにおいて、モデルを `edit_file` より `structural_edit` へバイアスする残差ストリームのステアリングベクトルで、文法ゲートが何かを拒否する機会を持つ**前に**適用されます。厳密にオプションです — ATLAS はこれなしでも動作し続けますが、ツール選択バイアスがステアされないままになります。

`atlas-bootstrap.sh` はサービス起動後に自動的にビルドします。パイプラインは:

1. `build_cvector_prompts.py` が、コミット済みの `geometric-lens/asa_calibration/contrast_pairs.jsonl`（1000 ペア）をポジティブ / ネガティブのプロンプトファイルに変換する。
2. ブートストラップが `llama-server` を短時間停止し、`llama-cvector-generator` を `--method mean -ngl 99` 付きのワンショットコンテナとして実行し、`models/ast_edit_steering.gguf` に加えて、ベクトルがどのモデルに対してビルドされたかを記録する `models/ast_edit_steering.gguf.model` サイドカーマーカーを書き込み、その後 `llama-server` を再起動する。
3. `inference/entrypoint-v3.1.sh` が次回起動時にファイルを検出し、`.model` サイドカーマーカーが選択中のモデルと一致することを確認して、`llama-server` のコマンドラインに `--control-vector-scaled /models/ast_edit_steering.gguf:0.5` を追加する。マーカーが存在しない、または別のモデルを指しているベクトルは**無効のまま**になります（起動バナーが理由を報告します）— ベクトルは 1 つのモデルに紐づく残差空間のアーティファクトだからです。

16GB GPU での合計実時間: 約 5 分。ビルドはモデルが載っているのと同じハードウェア上で実行され、生成されるベクトルはモデル固有です（あるモデルのアーティファクトに対してビルドされた `ast_edit_steering.gguf` を、別のベースモデルを実行するホストに移動しないでください）。

**動作のオーバーライド**（調整したい場合に `.env` で設定）:

| 環境変数 | デフォルト | 効果 |
|---|---|---|
| `ATLAS_CONTROL_VECTOR` | `/models/ast_edit_steering.gguf` | パスのオーバーライド |
| `ATLAS_CONTROL_VECTOR_SCALE` | `0.5` | 保守的。バイアスが弱すぎる場合は 1.0〜1.5 に上げ、ツール以外のタスクが劣化する場合は 0.2 に向けて下げてください。 |
| `ATLAS_CONTROL_VECTOR_LAYER_RANGE` | (全レイヤー) | 2 つの整数（例: `"24 30"`）を渡すとレイヤー帯にスコープします。狭いほど安全ですが効果は弱まります。 |
| `ATLAS_CONTROL_VECTOR_ALLOW_UNVERIFIED` | `0` | `1` に設定すると、`.model` サイドカーマーカーが存在しない、または選択中のモデルと一致しない場合でもベクトルを適用します。自分でビルドし、一致すると分かっているベクトルにのみ使ってください。 |

**ローカルビルドが失敗する場合**（例: 古い `atlas-llama` イメージに cvector-generator がない、GPU の OOM、ランタイム取得時のネットワーク障害）、ブートストラップは [ATLAS HuggingFace データセット](https://huggingface.co/datasets/itigges22/ATLAS)からプレビルドの `ast_edit_steering.gguf` をダウンロードするフォールバックを取ります。それも失敗した場合、インストールは警告付きで完了します — `atlas doctor` はこのギャップを `fail` ではなく `warn` としてフラグします。

ビルドを完全にスキップするには、インストーラーの実行前に `ATLAS_BOOTSTRAP_SKIP_ASA=1` を設定してください。

手動での再ビルド（ペアの再キュレーション、別の `--method`、別のベースモデル）については、[`geometric-lens/asa_calibration/README.md`](../../../geometric-lens/asa_calibration/README.md) を参照してください。

---

## 次のステップ

- [CLI.md](../../CLI.md) — ATLAS 起動後の使い方
- [CONFIGURATION.md](../../CONFIGURATION.md) — すべての環境変数とチューニングオプション
- [TROUBLESHOOTING.md](./TROUBLESHOOTING.md) — よくある問題と解決方法
- [ARCHITECTURE.md](./ARCHITECTURE.md) — システム内部の仕組み
