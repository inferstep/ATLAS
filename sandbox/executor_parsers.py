"""The syntax checks that run in the executor's own process: YAML, HTML with its Jinja templates, and XML."""

from __future__ import annotations

import re
from html.parser import HTMLParser
from typing import List, Optional


def _looks_like_jinja_template(filename: Optional[str]) -> bool:
    """True only for files that are Jinja templates by convention: under a
    templates/ directory, or a .jinja/.jinja2 name. Vue/Angular/Handlebars HTML
    shares `{{ }}` but is NOT Jinja and lives elsewhere, so it is never handed
    to a Jinja parser."""
    if not filename:
        return False
    f = filename.replace("\\", "/").lower()
    return ("templates/" in f) or f.endswith((".jinja", ".jinja2"))


def _jinja_template_errors(code: str, filename: Optional[str]) -> List[str]:
    """Jinja syntax errors for a file that is actually a Jinja template, else
    []. Requires a statement tag `{%` to be present -- `{{ }}` interpolation
    alone is too widely shared to attribute to Jinja. Unknown-tag/filter/test
    errors are dropped: those signal a third-party extension this parser has
    not loaded, not a typo. Never raises."""
    if not _looks_like_jinja_template(filename):
        return []
    if "{%" not in code:
        return []
    try:
        import jinja2
    except Exception:
        return []  # fail open: no Jinja available here
    try:
        jinja2.Environment(autoescape=True).parse(code)  # parsed only, never rendered
        return []
    except jinja2.TemplateSyntaxError as e:
        msg = (getattr(e, "message", "") or "").lower()
        if any(s in msg for s in ("unknown tag", "no filter named",
                                  "no test named", "not registered")):
            return []
        loc = f" (line {e.lineno})" if getattr(e, "lineno", None) else ""
        return [f"TemplateSyntaxError: {e.message}{loc}"]
    except Exception:
        return []  # not a confident syntax verdict -- stay silent


class _StructureParser(HTMLParser):
    """Counts the markup html.parser saw."""

    def __init__(self):
        super().__init__()
        self.markup = 0

    def handle_starttag(self, tag, attrs):
        self.markup += 1

    def handle_startendtag(self, tag, attrs):
        self.markup += 1

    def handle_decl(self, decl):
        self.markup += 1


def _html_structure_errors(code: str) -> List[str]:
    """What can be said of HTML without rejecting what HTML allows.

    html.parser accepts any text -- JavaScript, Python, an empty string, a
    page cut off inside its <script> -- so a clean parse proved nothing, and
    it was counted as a syntax pass for completion. Two checks can fail: the
    text has markup at all (a template made only of {% %} or {{ }} blocks is
    exempt), and it does not end inside a tag, a comment, or a <script> or
    <style> element, which is what a cut-off generation leaves. Unclosed
    ordinary elements, fragments and Vue or Angular templates still pass.
    """
    parser = _StructureParser()
    try:
        parser.feed(code)
    except Exception as e:
        return [str(e)]
    errors = []
    unclosed = getattr(parser, "cdata_elem", None)
    leftover = getattr(parser, "rawdata", "")
    if unclosed:
        errors.append(f"the document ends inside an unclosed <{unclosed}> element")
    elif re.match(r"<[!/?A-Za-z]", leftover.lstrip()):
        errors.append("the document ends inside an unfinished tag or comment: "
                      + leftover.lstrip()[:60])
    if parser.markup == 0 and "{%" not in code and "{{" not in code:
        errors.append("no HTML markup: the text has no tags")
    return errors


def _yaml_syntax_errors(code: str) -> List[str]:
    """Every document of the YAML text, parsed."""
    errors = []
    try:
        import yaml
        # safe_load_all, not safe_load: a multi-document file ("a: 1"
        # --- "b: 2") is valid YAML and extremely common in Kubernetes
        # and Compose manifests, but safe_load rejects it with "expected
        # a single document in the stream". That false rejection is why
        # the proxy's direct-write path carried no syntax gate at all,
        # which let genuinely unparseable files reach disk. list() forces
        # the generator so every document is actually parsed.
        list(yaml.safe_load_all(code))
    except Exception as e:
        errors.append(str(e))
    return errors


def _html_syntax_errors(code: str, filename: Optional[str]) -> List[str]:
    """The HTML structure check, then the Jinja parse of a file that is a Jinja template."""
    errors = []
    errors.extend(_html_structure_errors(code))
    # A Jinja template can parse cleanly as HTML and still 500 on EVERY
    # render: `{% for x in xs %)` closes the tag with `)` instead of `}`.
    # html.parser sees only text and passes it; Flask compiles the template
    # on first render, so the server starts, the file imports and the page
    # returns 500 with a TemplateSyntaxError nothing upstream caught.
    errors.extend(_jinja_template_errors(code, filename))
    return errors


def _xml_syntax_errors(code: str) -> List[str]:
    """The XML text, parsed with defusedxml."""
    errors = []
    from defusedxml import ElementTree as ET
    from defusedxml.common import DefusedXmlException
    try:
        ET.fromstring(code)
    except (ET.ParseError, DefusedXmlException) as e:
        errors.append(str(e))
    return errors
