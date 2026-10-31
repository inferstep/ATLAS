<!-- source: README.md synced-through: 69f2deafee29f4669c9fba659cebec683da94630 -->
> **[English](../../../README.md)** | **[简体中文](../zh-CN/README.md)** | **[日本語](../ja/README.md)** | **한국어**

<p align="center">
  <img src="../../images/herodemo.gif" alt="ATLAS TUI 실행 모습"/><br/>
  <sub><i>ATLAS TUI 라이브 데모(10배속). V3 파이프라인이 파일 생성을 처리하는 모습.</i></sub>
</p>

<h1 align="center">A.T.L.A.S.</h1>
<p align="center"><b>Adaptive Test-time Learning and Autonomous Specialization</b></p>

<p align="center">
  <img src="https://img.shields.io/badge/version-V3.1.7-blue" alt="버전"/>
  <img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="라이선스"/>
  <img src="https://img.shields.io/badge/model-agnostic-green" alt="모델 독립적"/>
</p>

<p align="center">
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/test.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/test.yml?branch=main&label=tests" alt="테스트"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/install-test.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/install-test.yml?branch=main&label=install%20matrix" alt="설치 매트릭스"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/codeql.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/codeql.yml?branch=main&label=codeql" alt="CodeQL"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/container-scan.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/container-scan.yml?label=container%20scan" alt="컨테이너 스캔"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/verify-tags.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/verify-tags.yml?label=release%20signature" alt="릴리스 서명"/></a>
</p>


## 🌎 ATLAS란?

**ATLAS는 프런티어급 추론과 검증을 콤팩트한 오픈 모델에 가져다주는 로컬 코딩 에이전트입니다.** 모델을 둘러싼 시스템(계획 수립, 후보 생성, 품질 스코어링, 샌드박스 테스트, 수리)에 더 많은 지능을 배치하여, 더 작은 모델이 호스팅 API나 토큰당 과금 없이 본인 하드웨어만으로 실제 소프트웨어 작업을 해낼 수 있게 합니다.

## 💡 왜 ATLAS인가?

* **더 작은 모델에서 더 많은 것을 끌어냅니다.** ATLAS는 단일 생성에 의존하는 대신 모델 주위에 계획 수립, 후보 선택, 검증, 수리를 더합니다.
* **받아들이기 전에 검증합니다.** 생성된 코드는 격리된 실행 환경 안에서 컴파일·테스트·수정될 수 있습니다.
* **연산을 필요한 곳에 씁니다.** 단순한 편집은 짧은 경로를 타고, 어려운 작업일수록 더 많은 후보·추론·검증을 받습니다.
* **본인 모델을 돌립니다.** NVIDIA, AMD, Apple Silicon, Vulkan, CPU 지원 하드웨어에서 호환 GGUF 모델을 사용할 수 있습니다. 모델에는 렌즈 번들이 필요하며, 없는 모델은 `atlas lens build`로 만들 수 있습니다.
* **로컬에서 관리합니다.** ATLAS는 저장소나 프롬프트를 호스팅 모델 또는 ATLAS 운영 서비스에 의도적으로 업로드하지 않습니다. 샌드박스 명령은 기본적으로 외부 네트워크에 접근할 수 있으며, `ATLAS_SANDBOX_NET_INTERNAL=true`로 비활성화할 수 있습니다.
* **스택 전체를 소유합니다.** ATLAS는 오픈 소스이며 셀프 호스팅됩니다. 호스팅 모델이나 타사 모델 제공업체 API 키는 필요하지 않으며, 로컬 설치별 서비스 토큰이 ATLAS 서비스 간 통신을 인증합니다.

---

## 📰 최신 릴리스

현재 릴리스는 **[V3.1.7 "Maia"](https://github.com/inferstep/ATLAS/releases/tag/v3.1.7)**입니다. 릴리스 노트는 해당 릴리스 페이지와 [CHANGELOG](../../../CHANGELOG.md)에 있습니다.

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

<sub>매주 월요일 GitHub Actions로 업데이트됩니다.</sub>

---

## 🧱 ATLAS의 기능

1. **[atlas-tui](../../CLI.md)** - 네이티브 Bubbletea 터미널 UI이자 공식 채팅 클라이언트. 아무 프로젝트 디렉토리에서나 `atlas`를 입력해 실행합니다.
   - [라이브 파이프라인 뷰](../../CLI.md#panes) - 사이드 패널에서 V3 단계를 실시간으로 확인
   - [슬래시 명령](../../CLI.md#slash-commands) - `/add`, `/diff`, `/commit`, `/run`으로 로컬 파일 컨텍스트와 셸 호출 처리
   - [입력 모드](../../CLI.md#input-modes) - 채팅, `!bash`, `/slash` 모드와 힌트 드롭다운

2. **[atlas-proxy](../../ARCHITECTURE.md#3-atlas-proxy-outer-layer)** - 시스템을 오케스트레이션하는 Go 에이전트 루프.
   - [도구 호출 라우팅](../../ARCHITECTURE.md#tools) - 파일 작업을 복잡도 등급별로 분류
   - [문법 강제](../../ARCHITECTURE.md#grammar-enforcement) - GBNF 스키마로 출력을 예상 JSON 형식 쪽으로 강하게 유도하고, 잘못되거나 잘린 출력은 프록시에서 복구
   - [BiasBusters](../../ARCHITECTURE.md#tool-selection-bias-mitigations) - 구조적 코드 편집에서 모델을 `structural_edit` 쪽으로 밀어주는 네 가지 결합 완화책(설명, 문법 금지, 시스템 노트, ASA 스티어링)
   - [안전 제한](../../ARCHITECTURE.md#safety-limits) - 턴 상한, 토큰 예산, 타임아웃

3. **[V3 파이프라인](../../ARCHITECTURE.md#4-v3-pipeline-inner-layer)** - 단일 프롬프트를 검증된 후보로 바꾸는 멀티 페이즈 코드 생성.
   - [PlanSearch](../../ARCHITECTURE.md#pipeline-flow) - 제약 기반 구조화 계획
   - [DivSampling](../../ARCHITECTURE.md#pipeline-flow) - 온도와 전략에 걸친 다양한 후보 생성
   - [예산 티어](../../ARCHITECTURE.md#pipeline-flow) - 할당기가 과제마다 계산 티어를 고릅니다. 티어는 생성할 후보 수와 후보마다의 프롬프트·토큰 예산을 정합니다
   - [PR-CoT Repair](../../ARCHITECTURE.md#pipeline-flow) - 샌드박스의 오류 출력을 바탕으로 실패한 후보를 수리
   - [Refinement Loops](../../ARCHITECTURE.md#pipeline-flow) - 샌드박스 검증과 수정의 반복

4. **[Geometric Lens](../../ARCHITECTURE.md#5-geometric-lens)** - 모델 자체 임베딩 위에서 동작하는 에너지 기반 스코어링. 외부 오라클 불필요. (["Geometric Lens"란?](../../ARCHITECTURE.md#why-geometric-lens))
   - [C(x) Cost Field](../../ARCHITECTURE.md#scoring-models) - 후보 품질을 스코어링하는 모델 hidden-dim→512→128→1 MLP
   - [G(x) Quality Prediction](../../ARCHITECTURE.md#scoring-models) - 생성할 후보 수를 정하는 데 쓰이는 XGBoost 앙상블. 후보 선택 자체는 가장 낮은 C(x)로 합니다

5. **[Sandbox](../../ARCHITECTURE.md#6-sandbox)** - 빌드 검증을 위한 격리 실행 환경.
   - 다중 언어 실행: Python, Rust, Go, C, Shell 등. Rust, Go, C는 프로젝트에서 명령으로 실행할 수 있습니다. V3는 구문을 검사하고, 샌드박스의 `/execute`는 컴파일만 하고 아직 실행하지 못합니다 ([#163](https://github.com/inferstep/ATLAS/issues/163))
   - 스코어링 전 컴파일과 린팅
   - 생성된 테스트와 기존 테스트 스위트 모두 실행

6. **[llama-server](../../CONFIGURATION.md#6-llama-server)** - 소비자용 GPU 한 장에서의 로컬 LLM 추론.
   - GPU 가속 양자화 추론(Q6_K / Q4_K_M) - NVIDIA CUDA, AMD ROCm, Apple Metal(macOS 하이브리드), Vulkan; Intel SYCL은 로드맵에 있음
   - 토큰 수준의 문법 제약 디코딩
   - 셀프 임베딩 — lens에 별도 모델이 필요 없음

전체 문서(설정, 아키텍처, 구성, 문제 해결, 그리고 각 구성요소의 [연구 배경](../../SOURCES.md))는 [docs/](../../) 디렉토리에 있습니다.

---

## 🚀 시작하기

원샷 설치:
```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

계속 바뀌는 스크립트를 그대로 bash에 파이프하고 싶지 않다면? 같은 설치 프로그램을 더 신중하게 실행하는 두 가지 방법이 있습니다:
```bash
# Pinned to a release: script, checkout, and images all at the signed tag
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/v3.1.7/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.7 bash

# Review before running
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

스크립트는 배포판(Ubuntu, Debian, RHEL, Fedora, Rocky, Alma)과 GPU 벤더(NVIDIA → nvidia-container-toolkit; AMD → ROCm 디바이스 패스스루)를 감지해 적절한 런타임을 설치하고, 모델 가중치를 다운로드하며, ASA 스티어링 벡터를 빌드하고 스택을 기동합니다. 소요 시간은 10~30분 정도이며, 모델 다운로드가 병목입니다.

완료 후 아무 프로젝트 디렉토리에서나 `atlas`를 실행하세요.

**요구 사항**

| | |
|---|---|
| GPU | VRAM 16GB 이상. NVIDIA (CUDA, 지원(Supported)), AMD (ROCm, 커뮤니티 검증(Community-tested)), 또는 Apple Silicon (Metal, macOS 하이브리드, 지원); 그 외 대부분의 GPU는 Vulkan(프리뷰(Preview))으로 커버됩니다. 사전 빌드된 CUDA 이미지는 Blackwell(RTX 50xx)을 대상으로 하며, 구형 NVIDIA GPU는 일회성 로컬 재빌드가 필요합니다([SETUP.md § CUDA Compute Capability](../../SETUP.md#cuda-compute-capability-dockerfilev31) 참고). 지원 수준: [SUPPORT_MATRIX.md](../../SUPPORT_MATRIX.md); GPU 목록: [SETUP.md § Supported GPUs](../../SETUP.md#supported-gpus). 특정 모델이 본인 카드에 맞는지 가늠하려면 [내 GPU에는 무엇이 들어가는가?](../../TROUBLESHOOTING.md#what-fits-on-my-gpu)를 참고하세요. |
| 런타임 | Docker (NVIDIA: + nvidia-container-toolkit; AMD: 단독 Docker로 충분) |
| Python | 3.9 이상 |
| 디스크 | 약 20GB CUDA / 약 22GB ROCm (모델 가중치 + 컨테이너 이미지) |

Apple Silicon은 macOS 하이브리드 Metal 경로(추론은 네이티브 llama-server, 나머지 스택은 Docker — **[SETUP_MACOS.md](../../SETUP_MACOS.md)** 참고)를 통해 네이티브로 동작합니다. Intel Arc (SYCL)는 로드맵에 있습니다. 수동 설치 경로(Docker Compose, 베어메탈, K3s)와 부트스트랩 플래그 전체 목록은 **[SETUP.md](SETUP.md)**를 참고하세요.

---

## ⚠️ 알려진 제한 사항

- **Linux Docker 스택과 네이티브 macOS 경로.** NVIDIA(지원(Supported)), AMD ROCm(커뮤니티 검증(Community-tested)), Vulkan(프리뷰(Preview)) Docker 경로가 현재 존재합니다. Apple Silicon(지원)은 네이티브 macOS 하이브리드 Metal 경로([#32](https://github.com/inferstep/ATLAS/issues/32))로 동작합니다. Intel Arc / SYCL은 로드맵(Roadmap)입니다. 수준 정의: [SUPPORT_MATRIX.md](../../SUPPORT_MATRIX.md).
- **ATLAS에는 현재 벤치마크 결과가 없습니다.** V3.0 LiveCodeBench 수치는 철회되었고([CHANGELOG](../../../CHANGELOG.md#withdrawn-the-v30-livecodebench-result-746) 참조), 현재 릴리스의 능력·신뢰성 수치는 아직 측정되지 않았습니다. 수치는 보류 과제로 재측정한 뒤에만 공개하며, 모델별 결과는 [#28](https://github.com/inferstep/ATLAS/issues/28)에서 추적합니다.
- **렌즈의 신호는 아직 입증되지 않았습니다.** 기준 모델에서는 점수를 내지만 보정되지 않은 상태이므로 개입은 중립입니다. 3.1.6에서는 쓸 만한 신호를 주지 못했고, `dev`에서는 아직 측정하지 않았습니다 ([#281](https://github.com/inferstep/ATLAS/issues/281)).
- **복잡한 기능 추가는 일관성이 떨어질 수 있습니다.** 콤팩트 모델은 코드를 쓰기 전에 낯선 코드베이스 탐색으로 에이전트 턴을 소비하기도 합니다.
- **문법 제약 디코딩은 제약 없는 디코딩보다 느립니다.**

---

## 🗺️ 로드맵

계획 중이거나 진행 중인 작업은 [ATLAS Roadmap 보드](https://github.com/orgs/inferstep/projects/1)에서 확인할 수 있습니다.

---

## ❤️ ATLAS 후원하기

ATLAS는 대학생 한 명이 여가 시간에 소비자용 GPU 한 장으로 만들고 있습니다. 이 프로젝트가 유용했고 지속 가능하도록 돕고 싶으시다면 **[GitHub 후원](https://github.com/sponsors/itigges22)**을 고려해 주세요.

후원금은 다음에 직접 쓰입니다:

- **컴퓨트 & 하드웨어** - 더 빠른 벤치마크 반복을 위한 GPU 추가, 메인테이너가 감당할 수 없는 아키텍처(AMD ROCm, 고용량 VRAM 카드, 대형 모델 실험용 클라우드 대여)에 대한 접근.
- **기여자 바운티** - 실질적인 PR에 진짜 시간을 들인 외부 기여자에게 의미 있는 보상을 제공하여, ATLAS가 1인 개발 속도보다 빠르게 성장할 수 있게 합니다.
- **연구** - 향후 워크숍·학회 제출부터 논문 작성, 접근법을 검증하고 확장하는 협업까지, 아키텍처를 둘러싼 지속적인 학술 활동.
- **커뮤니티** - 문서, 사용자 채널, 교육 콘텐츠 등 ATLAS가 더 많은 개발자에게 닿고 기존 사용자를 더 잘 지원하도록 하는 커뮤니티와 플랫폼에 대한 지속적인 지원.

모든 후원자는 자신이 후원한 버전의 릴리스 노트에 이름이 실립니다.

---

## 🤝 기여하기

ATLAS는 오픈으로 개발되며 기여자와 핵심 메인테이너를 환영합니다. 버그 수정, 가속기 지원, 더 큰 하위 시스템 작업 모두 환영입니다.

버그를 찾았거나 벽에 부딪히셨나요? **[이슈를 열어 주세요](https://github.com/inferstep/ATLAS/issues)** — 수정까지 제출하실 필요는 없습니다. 버그 리포트와 피드백은 코드만큼이나 큰 도움이 됩니다.

가이드라인은 **[CONTRIBUTING.md](../../../CONTRIBUTING.md)**를, 코드베이스 구조 개요는 [저장소 맵](../../README.md#repository-map)을 참고하세요.

---

## 📄 라이선스

[GNU Affero General Public License v3.0 (AGPL-3.0)](../../../LICENSE)에 따라 라이선스가 부여됩니다.
