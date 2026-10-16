"""Every build of the model server names the processor level of its CPU part.

llama.cpp compiles its CPU part for the processor of the build machine unless
the build says otherwise. An image built that way can hold instructions that a
user's processor does not have, and the compile itself can fail on a runner
whose processor is newer than the builder's assembler knows.
"""
import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
BUILD_FILES = sorted(path for path in (ROOT / "inference").glob("Dockerfile*") if "cmake -B" in path.read_text(encoding="utf-8"))


def without_comments(text: str) -> str:
    """A build file as Docker reads it: a comment line is dropped, also between the lines of one command."""
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith("#"))


def configure_steps(text: str) -> list:
    """Each `cmake -B` command of a build file, with its continuation lines joined into one."""
    return re.findall(r"cmake -B \S+[^&\n]*", re.sub(r"\\\n", " ", without_comments(text)))


def test_the_four_images_of_the_model_server_are_the_build_files_that_are_read():
    assert [path.name for path in BUILD_FILES] == ["Dockerfile", "Dockerfile.rocm", "Dockerfile.v31", "Dockerfile.vulkan"], (
        "the build files of the model server under inference/ are not the four that this test knows. Fix: when a build "
        "file is added or removed, give this list the same names; a new one must name its processor level too.")


@pytest.mark.parametrize("path", BUILD_FILES, ids=lambda path: path.name)
def test_the_build_names_the_processor_level_of_the_cpu_part(path):
    text = path.read_text(encoding="utf-8")
    steps = configure_steps(text)
    assert steps, f"inference/{path.name} has no `cmake -B` step that this test can read; the form of the step has changed"
    for step in steps:
        assert re.search(r"(?<!\S)-DGGML_NATIVE=OFF(?!\S)", step), (
            f"inference/{path.name} configures llama.cpp without `-DGGML_NATIVE=OFF`: `{' '.join(step.split())}`. Then the "
            "CPU part is compiled for the processor of the build machine: the image may not start on a user's older "
            "processor, and the compile fails on a runner whose processor the builder's assembler does not know. Fix: "
            "add `-DGGML_NATIVE=OFF` to that step.")
    code = without_comments(text)
    for native in ("-DGGML_NATIVE=ON", "-march=native", "-mcpu=native", "-mtune=native"):
        assert native not in code, (
            f"inference/{path.name} has `{native}`, which builds for the processor of the build machine. Fix: remove it.")


def test_the_support_page_says_which_processor_the_images_need():
    page = " ".join((ROOT / "SUPPORT_MATRIX.md").read_text(encoding="utf-8").split())
    assert "images for amd64 need a processor with AVX2" in page, (
        "SUPPORT_MATRIX.md does not say that the model-server images for amd64 need a processor with AVX2. The build "
        "files set that level (`-DGGML_NATIVE=OFF`). Fix: say it on the page.")
