"""Each Gradle wrapper of the repository is the one that Gradle published.

`gradle-wrapper.jar` is a program that jobs of the pipeline run, and it is a binary file: a change of it shows in
a pull request as a new size and nothing more. So its SHA-256 is held here against the sum that Gradle published
for the Gradle version of the wrapper. The published sums stand in `gradle_wrapper_jar_sums.txt` beside this file,
one line for a version. A change of that list is a change of a file that configures the checks, and the integrity
check names it to a maintainer.

The settings of the wrapper are held as they are today: the distribution comes from Gradle's own address, its sum
is set, and the wrapper checks the address.

The wrapper reads its settings with Java's own reader of such files, and that reader takes more forms than the
plain `key=value`: another sign or a space between the key and the value, a key that is written with an escape, a
value that goes on in the next line. For a key that stands twice it takes the last one. A test that read the file
in its own way could hold one setting while the wrapper uses another. So the file has to be in the plain form
that `gradle wrapper` and Dependabot write, and a file in another form is red before anything else is held.

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
# The plain form of a settings file. A line is empty, a comment that starts with `#`, or `key=value`: a key of
# letters and digits, the equals sign, and a value with no space in it. Every character is a printable one of
# ASCII. In a value a backslash stands only before a colon, as the wrapper writes the colon of an address.
A_COMMENT = re.compile(r"#[ -~]*")
A_PAIR = re.compile(r"(?P<key>[A-Za-z][A-Za-z0-9]*)=(?P<value>[!-~]*)")
A_BACKSLASH_ALONE = re.compile(r"\\(?!:)")


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


def plain_settings(raw, name):
    """The settings of a file in the plain form as key -> value, and a finding for each line that is not in that
    form or that gives a key a second time. In the plain form Java's reader and this one read the same pairs."""
    settings, findings = {}, []
    for number, line in enumerate(raw.decode("ascii", errors="replace").split("\n"), 1):
        line = line[:-1] if line.endswith("\r") else line
        pair = A_PAIR.fullmatch(line)
        if not line or A_COMMENT.fullmatch(line):
            continue
        if not pair or A_BACKSLASH_ALONE.search(pair["value"]):
            findings.append(
                f"`{name}/{SETTINGS}`, line {number}, is not in the plain form: {ascii(line)[:120]}. The plain form is "
                "an empty line, a comment that starts with `#`, or `key=value` with a key of letters and digits, no "
                "space, and a backslash only before a colon. The wrapper reads this file with Java's own reader, "
                "which takes more forms than that, so this test could hold one setting while the wrapper uses "
                "another. Fix: write the line as `gradle wrapper` writes it.")
        elif pair["key"] in settings:
            findings.append(
                f"`{name}/{SETTINGS}`, line {number}, gives `{pair['key']}` a second time. Java's reader takes the "
                "last line of a key, so the line that a reader of the file sees first is not the one that the "
                "wrapper uses. Fix: keep one line for the key.")
        else:
            settings[pair["key"]] = pair["value"].replace("\\:", ":")
    return settings, findings


def wrapper_findings(folder, sums, name):
    """The findings for one wrapper folder: its settings, and its jar against the published sum of its version."""
    jar, settings_file = folder / JAR, folder / SETTINGS
    missing = [f"`{name}` has no `{one.name}`. A wrapper is the jar and its settings together, and one of them alone "
               "cannot be held. Fix: make the wrapper again with Gradle, or take the folder out."
               for one in (jar, settings_file) if not one.is_file()]
    if missing:
        return missing
    settings, not_plain = plain_settings(settings_file.read_bytes(), name)
    if not_plain:
        # Nothing else is said of such a file: what this test would read from it is not what the wrapper reads.
        return not_plain
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
    if not found:
        # The address names no version, so there is no sum to hold the jar against.
        return findings
    version, has = found["version"], hashlib.sha256(jar.read_bytes()).hexdigest()
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


def test_the_list_of_published_sums_reads():
    sums, findings = published(SUMS.read_text(encoding="utf-8"))
    assert not findings, "\n".join(findings)
    assert sums, f"`{SUMS.name}` has no line, so no jar is held against anything."


def test_each_wrapper_of_the_repository_is_the_published_one():
    sums, _ = published(SUMS.read_text(encoding="utf-8"))
    folders = wrapper_folders(subprocess.check_output(["git", "ls-files"], cwd=ROOT, text=True).splitlines())
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


def settings_text(without=(), **changed):
    """The good settings as the file has them, with some changed or left out."""
    keys = {key: value for key, value in {**GOOD, **changed}.items() if key not in without}
    return "".join(f"{key}={value}\n" for key, value in keys.items())


def findings_for_a_wrapper(tmp_path, settings, jar=A_JAR):
    """The findings for a wrapper made here, of this settings text and this jar. None: the wrapper has no such file."""
    folder = tmp_path / "gradle" / "wrapper"
    folder.mkdir(parents=True)
    if jar is not None:
        (folder / JAR).write_bytes(jar)
    if settings is not None:
        (folder / SETTINGS).write_bytes(settings.encode())
    return wrapper_findings(folder, SUMS_HERE, "gradle/wrapper")


@pytest.mark.parametrize("address", [
    pytest.param("https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip", id="the binaries, as today"),
    pytest.param("https\\://services.gradle.org/distributions/gradle-9.8.0-all.zip", id="the binaries and the sources"),
])
def test_the_published_jar_with_good_settings_has_no_finding(tmp_path, address):
    assert findings_for_a_wrapper(tmp_path, settings_text(distributionUrl=address)) == []


@pytest.mark.parametrize("jar, version", [
    pytest.param(A_JAR[:-1] + b"1", "9.8.0", id="one byte changed"),
    pytest.param(A_JAR + b"\x00", "9.8.0", id="one byte more"),
    pytest.param(A_JAR[1:], "9.8.0", id="one byte less"),
    pytest.param(b"", "9.8.0", id="empty"),
    pytest.param(b"another jar", "9.8.0", id="the jar of another version"),
    pytest.param(A_JAR, "9.8.1", id="settings of another version than the jar"),
])
def test_a_jar_that_is_not_the_published_one_of_the_version_in_the_settings_is_found(tmp_path, jar, version):
    settings = settings_text(distributionUrl=f"https\\://services.gradle.org/distributions/gradle-{version}-bin.zip")
    findings = findings_for_a_wrapper(tmp_path, settings, jar=jar)
    assert len(findings) == 1, findings
    assert f"is not the wrapper jar that Gradle published for {version}" in findings[0], findings
    assert SUMS_HERE[version] in findings[0]
    assert hashlib.sha256(jar).hexdigest() in findings[0]
    assert "Fix:" in findings[0]
    assert "not with this wrapper" in findings[0]


@pytest.mark.parametrize("version", ["9.7.0", "9.9", "10.0.0-rc-1"])
def test_a_version_with_no_line_is_found_and_the_message_says_what_to_do(tmp_path, version):
    settings = settings_text(distributionUrl=f"https\\://services.gradle.org/distributions/gradle-{version}-bin.zip")
    findings = findings_for_a_wrapper(tmp_path, settings)
    assert len(findings) == 1, findings
    assert "has no line for that version" in findings[0], findings
    assert f"`{version} <sum>`" in findings[0]
    assert PAGE in findings[0]
    assert "a maintainer adds one line" in findings[0]
    assert "in a pull request of its own that merges first" in findings[0]
    assert hashlib.sha256(A_JAR).hexdigest() in findings[0]


@pytest.mark.parametrize("settings, words", [
    pytest.param(settings_text(distributionUrl="https\\://example.org/distributions/gradle-9.8.0-bin.zip"),
                 "`distributionUrl` is", id="an address of another host"),
    pytest.param(settings_text(distributionUrl="https\\://services.gradle.org.example.org/distributions/gradle-9.8.0-bin.zip"),
                 "`distributionUrl` is", id="a host whose name starts as Gradle's"),
    pytest.param(settings_text(distributionUrl="https\\://example.org/services.gradle.org/distributions/gradle-9.8.0-bin.zip"),
                 "`distributionUrl` is", id="Gradle's host in the path of another"),
    pytest.param(settings_text(distributionUrl="http\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip"),
                 "`distributionUrl` is", id="http with no tls"),
    pytest.param(settings_text(distributionUrl="https\\://services.gradle.org/distributions-snapshots/gradle-9.8.0-bin.zip"),
                 "`distributionUrl` is", id="a snapshot"),
    pytest.param(settings_text(distributionUrl="https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip?from=elsewhere"),
                 "`distributionUrl` is", id="a query after the file"),
    pytest.param(settings_text(distributionUrl="https\\://services.gradle.org/distributions/../other/gradle-9.8.0-bin.zip"),
                 "`distributionUrl` is", id="a path that leaves the folder"),
    pytest.param(settings_text(distributionUrl="https\\://services.gradle.org/distributions/gradle-9.8.0-20261012000000+0000-bin.zip"),
                 "`distributionUrl` is", id="a nightly build"),
    pytest.param(settings_text(distributionUrl="https\\://services.gradle.org/distributions/gradle-9.8.0-src.zip"),
                 "`distributionUrl` is", id="the sources only"),
    pytest.param(settings_text(distributionUrl="https\\://user@services.gradle.org/distributions/gradle-9.8.0-bin.zip"),
                 "`distributionUrl` is", id="a user before the host"),
    pytest.param(settings_text(distributionUrl=""), "`distributionUrl` is", id="an empty address"),
    pytest.param(settings_text(without=("distributionUrl",)), "`distributionUrl` is `(not set)`", id="no address"),
    pytest.param(settings_text(without=("distributionSha256Sum",)), "`distributionSha256Sum` is not set", id="no sum"),
    pytest.param(settings_text(distributionSha256Sum=""), "`distributionSha256Sum` is not set", id="an empty sum"),
    pytest.param(settings_text(distributionSha256Sum="ab" * 31), "`distributionSha256Sum` is not set", id="a sum of 62 digits"),
    pytest.param(settings_text(distributionSha256Sum="AB" * 32), "`distributionSha256Sum` is not set", id="a sum in capitals"),
    pytest.param(settings_text(distributionSha256Sum="zz" * 32), "`distributionSha256Sum` is not set", id="a sum that is not hex"),
    pytest.param(settings_text(without=("validateDistributionUrl",)), "`validateDistributionUrl` is not `true`",
                 id="no check of the address"),
    pytest.param(settings_text(validateDistributionUrl="false"), "`validateDistributionUrl` is not `true`",
                 id="the check of the address set to false"),
    pytest.param(settings_text(validateDistributionUrl=""), "`validateDistributionUrl` is not `true`",
                 id="the check of the address set to nothing"),
    pytest.param(settings_text(validateDistributionUrl="True"), "`validateDistributionUrl` is not `true`",
                 id="the check of the address set to True"),
])
def test_a_setting_that_is_not_as_today_is_found(tmp_path, settings, words):
    findings = findings_for_a_wrapper(tmp_path, settings)
    assert len(findings) == 1, findings
    assert words in findings[0], findings
    assert "Fix:" in findings[0], findings


def test_each_wrong_setting_has_a_finding_of_its_own(tmp_path):
    settings = settings_text(without=("distributionSha256Sum", "validateDistributionUrl"))
    findings = findings_for_a_wrapper(tmp_path, settings, jar=b"changed")
    assert len(findings) == 3, findings


@pytest.mark.parametrize("jar, settings, missing", [
    pytest.param(A_JAR, None, SETTINGS, id="a jar with no settings"),
    pytest.param(None, settings_text(), JAR, id="settings with no jar"),
])
def test_a_wrapper_with_one_of_its_two_files_is_found(tmp_path, jar, settings, missing):
    findings = findings_for_a_wrapper(tmp_path, settings, jar=jar)
    assert len(findings) == 1, findings
    assert f"has no `{missing}`" in findings[0], findings


def test_every_folder_with_a_jar_or_with_settings_is_a_wrapper():
    tracked = ["a/gradle/wrapper/gradle-wrapper.jar", "a/gradle/wrapper/gradle-wrapper.properties", "a/gradlew",
               "b/gradle/wrapper/gradle-wrapper.jar", "c/gradle-wrapper.properties", "d/another.jar",
               "e/not-gradle-wrapper.jar", "gradle-wrapper.jar"]
    assert wrapper_folders(tracked) == [".", "a/gradle/wrapper", "b/gradle/wrapper", "c"]
    assert wrapper_folders([]) == []


@pytest.mark.parametrize("text, versions, words", [
    pytest.param("# a comment\n\n9.8.0 " + "a" * 64 + "\n9.8.1 " + "b" * 64 + "\n", ["9.8.0", "9.8.1"], [],
                 id="two versions, a comment and an empty line"),
    pytest.param("9.8.0 " + "a" * 64 + "\n9.8.0 " + "a" * 64 + "\n", ["9.8.0"], [], id="the same line twice"),
    pytest.param("9.8.0 " + "a" * 64 + "\n9.8.0 " + "b" * 64 + "\n", ["9.8.0"], ["two sums"], id="a version with two sums"),
    pytest.param("9.8.0 " + "a" * 63 + "\n", [], ["Line 1"], id="a sum of 63 digits"),
    pytest.param("9.8.0 " + "A" * 64 + "\n", [], ["Line 1"], id="a sum in capitals"),
    pytest.param("9.8.0  " + "a" * 64 + "\n", [], ["Line 1"], id="two spaces"),
    pytest.param("9.8.0\n" + "a" * 64 + "\n", [], ["Line 1", "Line 2"], id="the version and the sum on two lines"),
    pytest.param("gradle-9.8.0 " + "a" * 64 + "\n", [], ["Line 1"], id="a word before the version"),
    pytest.param("9.8.0 " + "a" * 64 + " # the wrapper\n", [], ["Line 1"], id="a comment after the sum"),
])
def test_the_list_takes_a_version_and_a_sum_on_a_line_and_nothing_else(text, versions, words):
    sums, findings = published(text)
    assert sorted(sums) == versions
    assert len(findings) == len(words), findings
    assert all(word in finding for word, finding in zip(words, findings)), findings


# The settings file: the plain form, and each form that Java's reader takes and that is not plain.

AS_GRADLE_WRITES_IT = """distributionBase=GRADLE_USER_HOME
distributionPath=wrapper/dists
distributionUrl=https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip
distributionSha256Sum=bafd5ce9cfaea0fbccfdc8439a1ac42fbd4cd9c89dc9a988228d8a2639a58e6c
networkTimeout=10000
retries=0
retryBackOffMs=500
validateDistributionUrl=true
zipStoreBase=GRADLE_USER_HOME
zipStorePath=wrapper/dists
"""
# The settings that the wrapper takes from that file.
ITS_SETTINGS = {
    "distributionBase": "GRADLE_USER_HOME", "distributionPath": "wrapper/dists",
    "distributionUrl": "https://services.gradle.org/distributions/gradle-9.8.0-bin.zip",
    "distributionSha256Sum": "bafd5ce9cfaea0fbccfdc8439a1ac42fbd4cd9c89dc9a988228d8a2639a58e6c",
    "networkTimeout": "10000", "retries": "0", "retryBackOffMs": "500", "validateDistributionUrl": "true",
    "zipStoreBase": "GRADLE_USER_HOME", "zipStorePath": "wrapper/dists",
}


@pytest.mark.parametrize("text, settings", [
    pytest.param(AS_GRADLE_WRITES_IT, ITS_SETTINGS, id="as Gradle 9.8.0 writes it"),
    pytest.param(AS_GRADLE_WRITES_IT.replace("9.8.0", "9.8.1"),
                 {**ITS_SETTINGS, "distributionUrl": "https://services.gradle.org/distributions/gradle-9.8.1-bin.zip"},
                 id="as Gradle 9.8.1 writes it"),
    pytest.param("#Mon Oct 12 16:00:00 UTC 2026\n" + AS_GRADLE_WRITES_IT, ITS_SETTINGS, id="with the date line of an older Gradle"),
    pytest.param(AS_GRADLE_WRITES_IT.replace("\n", "\r\n"), ITS_SETTINGS, id="with the line ends of Windows"),
    pytest.param(AS_GRADLE_WRITES_IT.replace("networkTimeout", "\n# a comment, with spaces\nnetworkTimeout"), ITS_SETTINGS,
                 id="with an empty line and a comment"),
    pytest.param(AS_GRADLE_WRITES_IT.rstrip("\n"), ITS_SETTINGS, id="with no line end at the end"),
])
def test_a_settings_file_in_the_plain_form_is_read_with_no_finding(text, settings):
    assert plain_settings(text.encode(), "gradle/wrapper") == (settings, [])


GOOD_TEXT = settings_text()
URL_LINE = "distributionUrl=https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip\n"
ELSEWHERE = "https\\://example.org/distributions/gradle-9.8.0-bin.zip"
# Each row: bytes that Java's reader takes as settings, and that give the wrapper another address, or no check of
# the address, than a reader of plain `key=value` lines sees. The last rows are forms that change nothing for
# Java's reader; they are refused all the same, so that the rule stays one rule.
NOT_PLAIN = {
    "a second line for a key, with a colon": GOOD_TEXT + "validateDistributionUrl:false\n",
    "a second line for a key, with a space": GOOD_TEXT + "validateDistributionUrl false\n",
    "a second line for a key, with a tab": GOOD_TEXT + "validateDistributionUrl\tfalse\n",
    "a second line for the address, with a colon": GOOD_TEXT + f"distributionUrl:{ELSEWHERE}\n",
    "a second line whose key is written with an escape": GOOD_TEXT + "validateDistributionUr\\u006c=false\n",
    "a second line whose key has a backslash in it": GOOD_TEXT + "validate\\DistributionUrl=false\n",
    "a second line with spaces before the key": GOOD_TEXT + "  validateDistributionUrl=false\n",
    "a second line with a space before the equals sign": GOOD_TEXT + "validateDistributionUrl =false\n",
    "a second line after a carriage return in a comment": GOOD_TEXT + "#a note\rvalidateDistributionUrl=false\n",
    "a second line after a form feed": GOOD_TEXT + "\x0cvalidateDistributionUrl=false\n",
    "the one line of a key, with a space as the separator": settings_text(without=("validateDistributionUrl",)) + "validateDistributionUrl true\n",
    "the one line of the address, with a space as the separator": settings_text(without=("distributionUrl",)) + URL_LINE.replace("=", " ", 1),
    "a value that goes on in the next line": GOOD_TEXT.replace("/distributions/gradle-9.8.0-bin.zip", "/distributions/\\\n    gradle-9.8.0-bin.zip"),
    "a value with a space after it": settings_text(validateDistributionUrl="true "),
    "a value that is written with an escape": settings_text(validateDistributionUrl="\\u0074rue"),
    "a backslash before the backslash of the colon": GOOD_TEXT.replace("https\\:", "https\\\\:"),
    "a value with a letter that is not ASCII": settings_text(distributionBase="GRADLE_USER_HOM\u00c9"),
    "a key with a sign in it": GOOD_TEXT + "validate.DistributionUrl=false\n",
    "a line with no key": GOOD_TEXT + "=false\n",
    "a comment that starts with an exclamation mark": GOOD_TEXT + "!validateDistributionUrl=false\n",
    "a comment with spaces before it": GOOD_TEXT + "  # a note\n",
    "a value with a space before it": settings_text(validateDistributionUrl=" true"),
}


@pytest.mark.parametrize("text", NOT_PLAIN.values(), ids=NOT_PLAIN.keys())
def test_a_settings_file_that_is_not_in_the_plain_form_is_found(tmp_path, text):
    findings = findings_for_a_wrapper(tmp_path, text)
    assert findings, findings
    assert all("is not in the plain form" in finding and "Fix:" in finding for finding in findings), findings
    assert all("Java's own reader" in finding for finding in findings)


@pytest.mark.parametrize("line, key", [
    ("validateDistributionUrl=false\n", "validateDistributionUrl"),
    (f"distributionUrl={ELSEWHERE}\n", "distributionUrl"),
    (URL_LINE, "distributionUrl"),
    ("distributionSha256Sum=" + "cd" * 32 + "\n", "distributionSha256Sum"),
], ids=["the check of the address", "the address", "the same address again", "the sum of the distribution"])
def test_a_key_that_stands_twice_is_found(tmp_path, line, key):
    for place, text in enumerate((GOOD_TEXT + line, line + GOOD_TEXT)):
        findings = findings_for_a_wrapper(tmp_path / str(place), text)
        assert len(findings) == 1, findings
        assert f"gives `{key}` a second time" in findings[0], findings
        assert "Fix:" in findings[0], findings


def test_a_file_that_is_not_plain_gets_no_other_finding(tmp_path):
    # The jar is another one too, and nothing is said of it: what the test would read is not what the wrapper reads.
    findings = findings_for_a_wrapper(tmp_path, GOOD_TEXT + "validateDistributionUrl:false\n", jar=b"changed")
    assert len(findings) == 1, findings
    assert "line 5, is not in the plain form" in findings[0], findings
    assert "'validateDistributionUrl:false'" in findings[0]
