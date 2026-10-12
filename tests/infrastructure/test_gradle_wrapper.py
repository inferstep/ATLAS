"""Each Gradle wrapper of the repository is the one that Gradle published.

`gradle-wrapper.jar` is a program that jobs of the pipeline run, and it is a binary file: a change of it shows in
a pull request as a new size and nothing more. So its SHA-256 is held here against the sum that Gradle published
for the Gradle version of the wrapper. The published sums stand in `gradle_wrapper_jar_sums.txt` beside this file,
one line for a version. A change of that list is a change of a file that configures the checks, and the integrity
check names it to a maintainer.

The settings of the wrapper are held as they are today: the distribution comes from Gradle's own address, its sum
is set, and the wrapper checks the address.

No network is used. A version with no line in the list is red, and the message says what to do.
"""
import hashlib
import re
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SUMS = Path(__file__).with_name("gradle_wrapper_jar_sums.txt")
PAGE = "https://gradle.org/release-checksums/"
JAR, SETTINGS = "gradle-wrapper.jar", "gradle-wrapper.properties"
VERSION = r"\d+\.\d+(?:\.\d+)?(?:-(?:rc|milestone)-\d+)?"
ADDRESS = re.compile(rf"https://services\.gradle\.org/distributions/gradle-(?P<version>{VERSION})-(?:bin|all)\.zip")
A_SUM = re.compile(r"[0-9a-f]{64}")
LINE = re.compile(rf"(?P<version>{VERSION}) (?P<sum>[0-9a-f]{{64}})")


def published(text):
    """The list as version -> sum, and a finding for each line that is not `version sum` or that gives a version a
    second sum."""
    sums, findings = {}, []
    for number, raw in enumerate(text.splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        found = LINE.fullmatch(line)
        if not found:
            findings.append(f"Line {number} of `{SUMS.name}` is not a Gradle version and a SHA-256 of 64 hex "
                            f"digits in small letters, with one space between them: `{line}`. So no jar can be "
                            f"held against it. Fix: write the line as `9.8.0 <sum>`, with the sum from {PAGE}")
        elif sums.setdefault(found["version"], found["sum"]) != found["sum"]:
            findings.append(f"`{SUMS.name}` gives Gradle {found['version']} two sums (line {number} is the second). "
                            f"Gradle published one wrapper jar for a version. Fix: keep the line that {PAGE} "
                            "confirms, and take the other out.")
    return sums, findings


def settings_of(text):
    """The keys of a Java properties file as the wrapper writes it: `key=value`, with `\\:` for a colon."""
    pairs = (line.split("=", 1) for line in text.splitlines() if "=" in line and not line.lstrip().startswith(("#", "!")))
    return {key.strip(): value.strip().replace("\\:", ":").replace("\\=", "=") for key, value in pairs}


def settings_findings(name, settings):
    """What is held of the settings, and the Gradle version that they name (None when the address does not say)."""
    findings = []
    address = settings.get("distributionUrl", "")
    found = ADDRESS.fullmatch(address)
    if not found:
        findings.append(
            f"`{name}/{SETTINGS}`: `distributionUrl` is `{address or '(not set)'}`. It has to be Gradle's own "
            "address with one released version, `https\\://services.gradle.org/distributions/gradle-<version>-bin.zip` "
            "(or `-all.zip`), so that the jobs take Gradle from where Gradle publishes it and the jar can be held "
            "against that version's sum. Fix: set the address in that form.")
    if not A_SUM.fullmatch(settings.get("distributionSha256Sum", "")):
        findings.append(
            f"`{name}/{SETTINGS}`: `distributionSha256Sum` is not set to a SHA-256 of 64 hex digits. With no sum "
            "the wrapper does not check the distribution that it downloads. Fix: set it to the \"Binary-only (-bin) "
            f"ZIP Checksum\" of the version (or the one of `-all`), from {PAGE}")
    if settings.get("validateDistributionUrl") != "true":
        findings.append(
            f"`{name}/{SETTINGS}`: `validateDistributionUrl` is not `true`. With it the wrapper checks the address "
            "of the distribution before it downloads. Fix: set `validateDistributionUrl=true`.")
    return findings, found["version"] if found else None


def wrapper_findings(folder, sums, name):
    """The findings for one wrapper folder: its settings, and its jar against the published sum of its version."""
    jar, settings = folder / JAR, folder / SETTINGS
    missing = [f"`{name}` has no `{one.name}`. A wrapper is the jar and its settings together, and one of them alone "
               "cannot be held. Fix: make the wrapper again with Gradle, or take the folder out."
               for one in (jar, settings) if not one.is_file()]
    if missing:
        return missing
    findings, version = settings_findings(name, settings_of(settings.read_text(encoding="utf-8")))
    if version is None:
        return findings
    has = hashlib.sha256(jar.read_bytes()).hexdigest()
    if version not in sums:
        findings.append(
            f"`{name}/{JAR}` is for Gradle {version}, and `{SUMS.name}` has no line for that version. So the jar is "
            f"held against nothing. Fix: a maintainer adds one line, `{version} <sum>`, with the \"Wrapper JAR "
            f"Checksum\" of {version} from {PAGE} , in a pull request of its own that merges first. The jar here "
            f"has the sum {has}; add the line only when Gradle's page gives the same sum.")
    elif sums[version] != has:
        findings.append(
            f"`{name}/{JAR}` is not the wrapper jar that Gradle published for {version}: its SHA-256 is {has}, and "
            f"the published one is {sums[version]}. Fix: make the wrapper again with a Gradle that is installed "
            f"on its own (`gradle wrapper --gradle-version {version}`), not with this wrapper, and commit the jar "
            "that it writes. When the settings name another version than the jar is for, bring the two together.")
    return findings


def wrapper_folders(tracked):
    """The folders that hold a wrapper jar or its settings, from the names of the tracked files."""
    return sorted({str(Path(name).parent) for name in tracked if Path(name).name in (JAR, SETTINGS)})


def tracked_files():
    return subprocess.check_output(["git", "ls-files"], cwd=ROOT, text=True).splitlines()


def test_the_list_of_published_sums_reads():
    sums, findings = published(SUMS.read_text(encoding="utf-8"))
    assert not findings, "\n".join(findings)
    assert sums, f"`{SUMS.name}` has no line, so no jar is held against anything."


def test_each_wrapper_of_the_repository_is_the_published_one():
    sums, _ = published(SUMS.read_text(encoding="utf-8"))
    folders = wrapper_folders(tracked_files())
    assert folders, ("No Gradle wrapper was found among the tracked files, so this test holds nothing. Fix: when "
                     "the repository has no Gradle build any more, take this file and its list out.")
    findings = [finding for name in folders for finding in wrapper_findings(ROOT / name, sums, name)]
    assert not findings, "\n".join(findings)


# The same findings on wrappers made here, so that a check which sees nothing cannot let the test above pass.

A_JAR = b"PK\x03\x04 a stand-in for the wrapper jar of Gradle 9.8.0"
SUMS_HERE = {"9.8.0": hashlib.sha256(A_JAR).hexdigest(), "9.8.1": hashlib.sha256(b"another jar").hexdigest()}
GOOD = {
    "distributionBase": "GRADLE_USER_HOME",
    "distributionUrl": "https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip",
    "distributionSha256Sum": "ab" * 32,
    "validateDistributionUrl": "true",
}


def a_wrapper(tmp_path, jar=A_JAR, without=(), **changed):
    folder = tmp_path / "gradle" / "wrapper"
    folder.mkdir(parents=True)
    if jar is not None:
        (folder / JAR).write_bytes(jar)
    if changed.get("settings", True) is not None:
        keys = {key: value for key, value in {**GOOD, **changed}.items() if key not in without}
        (folder / SETTINGS).write_text("".join(f"{key}={value}\n" for key, value in keys.items()), encoding="utf-8")
    return wrapper_findings(folder, SUMS_HERE, "gradle/wrapper")


def test_the_published_jar_with_the_settings_of_today_has_no_finding(tmp_path):
    assert a_wrapper(tmp_path) == []


def test_the_all_distribution_of_the_same_version_has_no_finding(tmp_path):
    assert a_wrapper(tmp_path, distributionUrl="https\\://services.gradle.org/distributions/gradle-9.8.0-all.zip") == []


@pytest.mark.parametrize("jar", [
    A_JAR[:-1] + b"1", A_JAR + b"\x00", A_JAR[1:], b"", b"another jar",
], ids=["one byte changed", "one byte more", "one byte less", "empty", "the jar of another version"])
def test_a_jar_that_is_not_the_published_one_is_found(tmp_path, jar):
    findings = a_wrapper(tmp_path, jar=jar)
    assert len(findings) == 1 and "is not the wrapper jar that Gradle published for 9.8.0" in findings[0], findings
    assert SUMS_HERE["9.8.0"] in findings[0] and hashlib.sha256(jar).hexdigest() in findings[0]
    assert "Fix:" in findings[0] and "not with this wrapper" in findings[0]


def test_the_jar_of_one_version_with_the_settings_of_another_is_found(tmp_path):
    findings = a_wrapper(tmp_path, distributionUrl="https\\://services.gradle.org/distributions/gradle-9.8.1-bin.zip")
    assert len(findings) == 1 and "published for 9.8.1" in findings[0], findings


@pytest.mark.parametrize("version", ["9.7.0", "9.9", "10.0.0-rc-1"])
def test_a_version_with_no_line_is_found_and_the_message_says_what_to_do(tmp_path, version):
    findings = a_wrapper(tmp_path, distributionUrl=f"https\\://services.gradle.org/distributions/gradle-{version}-bin.zip")
    assert len(findings) == 1 and "has no line for that version" in findings[0], findings
    assert f"`{version} <sum>`" in findings[0] and PAGE in findings[0] and "a maintainer adds one line" in findings[0]
    assert "in a pull request of its own that merges first" in findings[0]
    assert hashlib.sha256(A_JAR).hexdigest() in findings[0]


@pytest.mark.parametrize("address", [
    "https\\://example.org/distributions/gradle-9.8.0-bin.zip",
    "https\\://services.gradle.org.example.org/distributions/gradle-9.8.0-bin.zip",
    "https\\://example.org/services.gradle.org/distributions/gradle-9.8.0-bin.zip",
    "http\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip",
    "https\\://services.gradle.org/distributions-snapshots/gradle-9.8.0-bin.zip",
    "https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip?from=elsewhere",
    "https\\://services.gradle.org/distributions/../other/gradle-9.8.0-bin.zip",
    "https\\://services.gradle.org/distributions/gradle-9.8.0-20261012000000+0000-bin.zip",
    "https\\://services.gradle.org/distributions/gradle-9.8.0-src.zip",
    "https\\://user@services.gradle.org/distributions/gradle-9.8.0-bin.zip",
    "",
])
def test_an_address_that_is_not_gradles_own_for_one_version_is_found(tmp_path, address):
    findings = a_wrapper(tmp_path, distributionUrl=address)
    assert len(findings) == 1 and "`distributionUrl` is" in findings[0] and "Fix:" in findings[0], findings


def test_settings_with_no_address_are_found(tmp_path):
    findings = a_wrapper(tmp_path, without=("distributionUrl",))
    assert len(findings) == 1 and "`distributionUrl` is `(not set)`" in findings[0], findings


@pytest.mark.parametrize("change", [
    {"without": ("distributionSha256Sum",)}, {"distributionSha256Sum": ""}, {"distributionSha256Sum": "ab" * 31},
    {"distributionSha256Sum": "AB" * 32}, {"distributionSha256Sum": "zz" * 32},
])
def test_a_distribution_sum_that_is_not_set_is_found(tmp_path, change):
    findings = a_wrapper(tmp_path, **change)
    assert len(findings) == 1 and "`distributionSha256Sum` is not set" in findings[0] and "Fix:" in findings[0], findings


@pytest.mark.parametrize("change", [
    {"without": ("validateDistributionUrl",)}, {"validateDistributionUrl": "false"}, {"validateDistributionUrl": ""},
    {"validateDistributionUrl": "True"},
])
def test_a_wrapper_that_does_not_check_the_address_is_found(tmp_path, change):
    findings = a_wrapper(tmp_path, **change)
    assert len(findings) == 1 and "`validateDistributionUrl` is not `true`" in findings[0], findings


def test_each_wrong_setting_has_a_finding_of_its_own(tmp_path):
    findings = a_wrapper(tmp_path, jar=b"changed", without=("distributionSha256Sum", "validateDistributionUrl"))
    assert len(findings) == 3, findings


def test_a_jar_with_no_settings_and_settings_with_no_jar_are_found(tmp_path):
    no_settings = a_wrapper(tmp_path / "a", settings=None)
    no_jar = a_wrapper(tmp_path / "b", jar=None)
    assert len(no_settings) == 1 and f"has no `{SETTINGS}`" in no_settings[0], no_settings
    assert len(no_jar) == 1 and f"has no `{JAR}`" in no_jar[0], no_jar


def test_every_folder_with_a_jar_or_with_settings_is_a_wrapper():
    tracked = ["a/gradle/wrapper/gradle-wrapper.jar", "a/gradle/wrapper/gradle-wrapper.properties", "a/gradlew",
               "b/gradle/wrapper/gradle-wrapper.jar", "c/gradle-wrapper.properties", "d/another.jar",
               "e/not-gradle-wrapper.jar", "gradle-wrapper.jar"]
    assert wrapper_folders(tracked) == [".", "a/gradle/wrapper", "b/gradle/wrapper", "c"]
    assert wrapper_folders([]) == []


@pytest.mark.parametrize("text, versions, words", [
    ("# a comment\n\n9.8.0 " + "a" * 64 + "\n9.8.1 " + "b" * 64 + "\n", ["9.8.0", "9.8.1"], []),
    ("9.8.0 " + "a" * 64 + "\n9.8.0 " + "a" * 64 + "\n", ["9.8.0"], []),
    ("9.8.0 " + "a" * 64 + "\n9.8.0 " + "b" * 64 + "\n", ["9.8.0"], ["two sums"]),
    ("9.8.0 " + "a" * 63 + "\n", [], ["Line 1"]),
    ("9.8.0 " + "A" * 64 + "\n", [], ["Line 1"]),
    ("9.8.0  " + "a" * 64 + "\n", [], ["Line 1"]),
    ("9.8.0\n" + "a" * 64 + "\n", [], ["Line 1", "Line 2"]),
    ("gradle-9.8.0 " + "a" * 64 + "\n", [], ["Line 1"]),
    ("9.8.0 " + "a" * 64 + " # the wrapper\n", [], ["Line 1"]),
])
def test_the_list_takes_a_version_and_a_sum_on_a_line_and_nothing_else(text, versions, words):
    sums, findings = published(text)
    assert sorted(sums) == versions
    assert len(findings) == len(words) and all(word in finding for word, finding in zip(words, findings)), findings


def test_the_settings_are_read_as_the_wrapper_writes_them():
    text = "#Mon Oct 12\ndistributionUrl=https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip\nretries=0\n"
    assert settings_of(text) == {
        "distributionUrl": "https://services.gradle.org/distributions/gradle-9.8.0-bin.zip", "retries": "0"}
