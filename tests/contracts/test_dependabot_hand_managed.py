"""A dependency that the Dependabot settings keep for a maintainer is named there as Dependabot names it.

An ignore rule of `.github/dependabot.yml` that names nothing does nothing, and nothing says so: Dependabot goes on
with its pull requests for the dependency that was meant. A rule with the long name of a Kotlin plugin
(`org.jetbrains.kotlin.jvm`) is such a rule, because Dependabot calls that plugin `jvm`. So each rule of the entry
for the JetBrains plugin is held here against the names that the build file has, as Dependabot reads them.

The Kotlin plugins of that build are held the other way round too: each has a rule for every version. The Kotlin
compiler follows the platform that the plugin targets, so all of them move together and by hand. A Kotlin plugin
with no rule, or with a rule for some versions only, would be moved alone.

The names follow Dependabot's own reader of a Gradle build (`gradle/lib/dependabot/gradle/file_parser.rb` of
dependabot-core, as read on 2026-10-12). A line of the `plugins` block with `kotlin("x")` or `id("x")` and a
version is the dependency `x`. A text `"group:name:version"` is the dependency `group:name`. A plugin line with no
version is no dependency, and a text with two parts is none: the build file names what it excludes in that form.
"""
import re
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
SETTINGS = ROOT / ".github" / "dependabot.yml"
FOLDER = "/extensions/jetbrains"
BUILD = ROOT / "extensions" / "jetbrains" / "build.gradle.kts"
# The Gradle plugin that the comment of the entry names as managed by hand, by the name that Dependabot gives it.
PLATFORM_PLUGIN = "org.jetbrains.intellij.platform"

# What Dependabot takes out before it reads: a comment that starts a line or follows a space.
LINE_COMMENT = re.compile(r"(?:^|(?<=\s))//.*$", re.MULTILINE)
BLOCK_COMMENT = re.compile(r"(?:^|(?<=\s))/\*.*?\*/", re.MULTILINE | re.DOTALL)
PART = r"""[^\s,@'":/\\]+"""
VERSION_PART = r"""[^\s,'":/\\]+"""
PLUGINS_BLOCK = re.compile(r"(?:^|\s)plugins\s*\{", re.MULTILINE)
PLUGIN = re.compile(rf"""(?P<kind>id|kotlin)(?:\s+['"](?P<spaced>{PART})['"]|\(['"](?P<called>{PART})['"]\))""")
PLUGIN_VERSION = re.compile(rf"""version\s+['"]?{VERSION_PART}['"]?""")
LIBRARY = re.compile(rf"""(?:\(|\s)\s*['"](?P<group>{PART}):(?P<name>{PART}):{VERSION_PART}['"]""")


def with_no_comment(build):
    return BLOCK_COMMENT.sub("", LINE_COMMENT.sub("\n", build))


def plugin_blocks(text):
    """The text of each `plugins { ... }` block, up to the brace that closes it."""
    for start in PLUGINS_BLOCK.finditer(text):
        rest, open_braces = text[start.end():], 1
        for index, char in enumerate(rest):
            open_braces += (char == "{") - (char == "}")
            if not open_braces:
                yield rest[:index]
                break


def is_kotlin(kind, name):
    return kind == "kotlin" or name.startswith("org.jetbrains.kotlin.")


def dependencies_of(build):
    """Each dependency of a build file by the name that Dependabot gives it, with its kind: `kotlin plugin`,
    `plugin` or `library`."""
    text, found = with_no_comment(build), {}
    for block in plugin_blocks(text):
        for line in block.splitlines():
            plugin = PLUGIN.search(line)
            if plugin and PLUGIN_VERSION.search(line):
                name = plugin["spaced"] or plugin["called"]
                found[name] = "kotlin plugin" if is_kotlin(plugin["kind"], name) else "plugin"
    for library in LIBRARY.finditer(text):
        found.setdefault(f"{library['group']}:{library['name']}", "library")
    return found


def names(rule, dependency):
    """Whether a rule's `dependency-name` names a dependency. `*` stands for any text, and the case does not
    count, as with Dependabot."""
    pattern = ".*".join(re.escape(part) for part in str(rule.get("dependency-name", "")).lower().split("*"))
    return re.fullmatch(pattern, dependency.lower()) is not None


def is_for_every_version(rule):
    return not rule.get("versions") and not rule.get("update-types")


def rules_of(settings, folder):
    """The ignore rules of the gradle entries for a folder."""
    return [rule for entry in settings.get("updates", [])
            if entry.get("package-ecosystem") == "gradle" and entry.get("directory") == folder
            for rule in entry.get("ignore") or []]


def rules_that_name_nothing(rules, dependencies):
    return [f"The rule `{rule.get('dependency-name')}` of the entry for {FOLDER} in `.github/dependabot.yml` names "
            f"no dependency of the build file. So it ignores nothing, and Dependabot goes on with its pull requests "
            f"for the dependency that was meant. Fix: write the name that Dependabot gives it. For a plugin that is "
            f"the word inside `kotlin(\"...\")` or `id(\"...\")`; for a library it is `group:name`, and `group:*` "
            f"for a whole group. Take the rule out when the build has the dependency no more. The build has: "
            f"{', '.join(sorted(dependencies)) or 'nothing that Dependabot reads'}."
            for rule in rules if not any(names(rule, dependency) for dependency in dependencies)]


def not_kept_for_a_maintainer(wanted, rules, why):
    """A finding for each wanted dependency that has no rule for every version."""
    findings = []
    for dependency in sorted(wanted):
        own = [rule for rule in rules if names(rule, dependency)]
        if any(is_for_every_version(rule) for rule in own):
            continue
        state = "has a rule for some versions only" if own else "has no rule"
        findings.append(
            f"`{dependency}` {state} in the entry for {FOLDER} of `.github/dependabot.yml`. {why} Fix: add "
            f"`- dependency-name: \"{dependency}\"` to the `ignore` list of that entry, with no `versions` and no "
            f"`update-types` line.")
    return findings


KOTLIN_WHY = ("It is a Kotlin plugin of the build, and the Kotlin plugins move together and by hand: the Kotlin "
              "compiler follows the platform that the plugin targets. So Dependabot would move this one alone.")
PLATFORM_WHY = ("The comment of the entry says that the IntelliJ Platform Gradle Plugin is managed by hand. So "
                "Dependabot would bump it all the same.")


@pytest.fixture(scope="module")
def rules():
    found = rules_of(yaml.safe_load(SETTINGS.read_text(encoding="utf-8")), FOLDER)
    assert found, (f"`.github/dependabot.yml` has no gradle entry for {FOLDER} with an `ignore` list, so nothing is "
                   "held here. Fix: when the entry moved to another folder, change FOLDER and BUILD in this file.")
    return found


@pytest.fixture(scope="module")
def dependencies():
    found = dependencies_of(BUILD.read_text(encoding="utf-8"))
    assert found, f"No dependency was read from `{BUILD.relative_to(ROOT)}`, so nothing is held here."
    return found


def test_each_rule_names_a_dependency_of_the_build(rules, dependencies):
    findings = rules_that_name_nothing(rules, dependencies)
    assert not findings, "\n".join(findings)


def test_each_kotlin_plugin_of_the_build_is_kept_at_every_version(rules, dependencies):
    kotlin = [name for name, kind in dependencies.items() if kind == "kotlin plugin"]
    assert kotlin, (f"No Kotlin plugin was read from `{BUILD.relative_to(ROOT)}`, so this test holds nothing. Fix: "
                    "when the build names its Kotlin plugins in a new form, teach `dependencies_of` that form.")
    findings = not_kept_for_a_maintainer(kotlin, rules, KOTLIN_WHY)
    assert not findings, "\n".join(findings)


def test_the_platform_gradle_plugin_is_kept_at_every_version(rules, dependencies):
    assert dependencies.get(PLATFORM_PLUGIN) == "plugin", (
        f"The build file has no plugin `{PLATFORM_PLUGIN}` with a version, so this test holds nothing. Fix: when "
        "the plugin has a new id, change PLATFORM_PLUGIN here and the rule in `.github/dependabot.yml`.")
    findings = not_kept_for_a_maintainer([PLATFORM_PLUGIN], rules, PLATFORM_WHY)
    assert not findings, "\n".join(findings)


# The reader and the two findings, on texts made here: a reader that reads nothing would let every test above pass.

A_BUILD = '''
import org.jetbrains.kotlin.gradle.dsl.KotlinVersion

plugins {
    // kotlin("plugin.allopen") version "2.3.20"
    kotlin("jvm") version "2.3.20"
    kotlin("plugin.compose") version "2.3.20"   // kotlin("plugin.noarg") version "2.3.20"
    kotlin("plugin.serialization") version "2.3.20"
    /* kotlin("plugin.spring") version "2.3.20" */
    id("org.jetbrains.intellij.platform") version "2.19.0"
    id("org.example.no.version")
    kotlin("kapt")
}

val ktorVersion = "3.4.3"
val provided = listOf("org.jetbrains.kotlin:kotlin-stdlib", "org.jetbrains.kotlinx:kotlinx-coroutines-core")

dependencies {
    implementation("io.ktor:ktor-client-core:$ktorVersion")
    testImplementation(platform("org.junit:junit-bom:5.13.4"))
    testImplementation("org.junit.jupiter:junit-jupiter")
    // implementation("com.example:commented-out:1.0")
}

ktlint {
    id("org.example.outside.the.block") version "1.0"
}
'''


def test_the_reader_gives_each_dependency_the_name_that_dependabot_gives_it():
    assert dependencies_of(A_BUILD) == {
        "jvm": "kotlin plugin", "plugin.compose": "kotlin plugin", "plugin.serialization": "kotlin plugin",
        "org.jetbrains.intellij.platform": "plugin",
        "io.ktor:ktor-client-core": "library", "org.junit:junit-bom": "library",
    }


@pytest.mark.parametrize("line, expected", [
    ('id "org.example.groovy" version "1.0"', {"org.example.groovy": "plugin"}),
    ("id 'org.example.groovy' version '1.0'", {"org.example.groovy": "plugin"}),
    ('id("org.jetbrains.kotlin.jvm") version "2.3.20"', {"org.jetbrains.kotlin.jvm": "kotlin plugin"}),
    # A version that is the name of a value counts as a version here. Dependabot looks the value up.
    ('kotlin("jvm") version kotlinVersion', {"jvm": "kotlin plugin"}),
    ('kotlin("jvm")', {}),
    ('// kotlin("jvm") version "2.3.20"', {}),
])
def test_the_reader_takes_a_plugin_line_as_dependabot_does(line, expected):
    assert dependencies_of(f"plugins {{\n    {line}\n}}\n") == expected


@pytest.mark.parametrize("name, named", [
    ("jvm", True), ("JVM", True), ("plugin.compose", True), ("plugin.*", True), ("io.ktor:*", True),
    ("io.ktor:ktor-client-core", True), ("*", True), ("org.junit:junit-bom", True),
    # The long names of the Kotlin plugins, which Dependabot does not use for `kotlin("...")`.
    ("org.jetbrains.kotlin.jvm", False), ("org.jetbrains.kotlin.plugin.compose", False),
    ("org.jetbrains.kotlin.plugin.serialization", False), ("org.jetbrains.kotlin*", False),
    # Named in the build with two parts only, as a thing that is excluded.
    ("org.jetbrains.kotlin:*", False), ("org.jetbrains.kotlinx:kotlinx-coroutines-core", False),
    # No version of its own, a commented line, a line outside the block, a part of a name.
    ("org.junit.jupiter:junit-jupiter", False), ("kapt", False), ("org.example.no.version", False),
    ("plugin.allopen", False), ("plugin.noarg", False), ("plugin.spring", False), ("com.example:*", False),
    ("org.example.outside.the.block", False), ("plugin", False), ("io.ktor", False), ("", False),
])
def test_a_rule_that_names_nothing_is_found(name, named):
    findings = rules_that_name_nothing([{"dependency-name": name}], dependencies_of(A_BUILD))
    assert (not findings) == named, findings
    if findings:
        assert f"The rule `{name}`" in findings[0] and "Fix:" in findings[0] and "plugin.compose" in findings[0]


@pytest.mark.parametrize("rules, left_alone", [
    ([{"dependency-name": "jvm"}, {"dependency-name": "plugin.compose"}, {"dependency-name": "plugin.serialization"}],
     []),
    ([{"dependency-name": "jvm"}, {"dependency-name": "plugin.*"}], []),
    ([{"dependency-name": "jvm"}, {"dependency-name": "plugin.compose"}], ["plugin.serialization"]),
    ([{"dependency-name": "org.jetbrains.kotlin.jvm"}, {"dependency-name": "plugin.*"}], ["jvm"]),
    ([{"dependency-name": "jvm", "versions": [">= 2.4.0"]}, {"dependency-name": "plugin.*"}], ["jvm"]),
    ([{"dependency-name": "jvm", "update-types": ["version-update:semver-major"]}, {"dependency-name": "plugin.*"}],
     ["jvm"]),
    ([{"dependency-name": "jvm", "versions": [">= 2.4.0"]}, {"dependency-name": "jvm"}, {"dependency-name": "plugin.*"}],
     []),
    ([], ["jvm", "plugin.compose", "plugin.serialization"]),
])
def test_a_kotlin_plugin_that_dependabot_would_move_alone_is_found(rules, left_alone):
    kotlin = [name for name, kind in dependencies_of(A_BUILD).items() if kind == "kotlin plugin"]
    findings = not_kept_for_a_maintainer(kotlin, rules, KOTLIN_WHY)
    assert [finding.split("`")[1] for finding in findings] == left_alone, findings
    assert all("Fix:" in finding and "move together" in finding for finding in findings)


def test_the_settings_are_read_for_the_one_folder_and_for_gradle_only():
    settings = {"updates": [
        {"package-ecosystem": "gradle", "directory": FOLDER, "ignore": [{"dependency-name": "jvm"}]},
        {"package-ecosystem": "gradle", "directory": "/another", "ignore": [{"dependency-name": "other.folder"}]},
        {"package-ecosystem": "pip", "directory": FOLDER, "ignore": [{"dependency-name": "torch"}]},
        {"package-ecosystem": "gradle", "directory": FOLDER},
    ]}
    assert rules_of(settings, FOLDER) == [{"dependency-name": "jvm"}]
    assert rules_of({}, FOLDER) == []
