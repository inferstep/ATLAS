#!/usr/bin/env python3
"""Render the repository's star-history chart as static SVGs.

Self-hosted replacement for the star-history.com embed (their shared
API-token pool rate-limits unpredictably). Writes one SVG per color
scheme; the README's <picture> element picks the right one. Stdlib only.

    GH_TOKEN=$(gh auth token) python3 scripts/star-history-chart.py [outdir]

Data sources, in order of preference:

  1. Per-star timestamps from /repos/{repo}/stargazers. Since July 2026
     GitHub limits this endpoint to repository admins and collaborators,
     so it works with a maintainer token but not with the workflow token
     in CI.
  2. The current stargazers_count from /repos/{repo} (readable with any
     token), appended as a dated sample to star-history.json alongside
     the SVGs.

Both paths persist the cumulative series to star-history.json and render
from it: a maintainer-token run rewrites the series at per-day
resolution, and the weekly CI run keeps it current by appending one
sample per run.
"""

import json
import math
import os
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone

REPO = os.environ.get("GITHUB_REPOSITORY", "inferstep/ATLAS")
SERIES_FILE = "star-history.json"

# Chart tokens per color scheme. Each SVG carries its own surface rect
# (GitHub's light/dark page colors) so it renders correctly anywhere,
# not only on the surface it was designed for; series + ink colors are
# validated against these surfaces.
MODES = {
    "light": {"surface": "#ffffff", "series": "#2a78d6", "ink": "#0b0b0b",
              "ink2": "#52514e", "grid": "#e8e8e6"},
    "dark":  {"surface": "#0d1117", "series": "#3987e5", "ink": "#ffffff",
              "ink2": "#c3c2b7", "grid": "#30363d"},
}

W, H = 800, 280
M_LEFT, M_RIGHT, M_TOP, M_BOT = 52, 84, 34, 36


def _api_json(url, token, accept="application/vnd.github+json"):
    req = urllib.request.Request(url, headers={
        "Accept": accept,
        "X-GitHub-Api-Version": "2022-11-28",
        **({"Authorization": f"Bearer {token}"} if token else {}),
    })
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def fetch_star_times(token):
    times = []
    page = 1
    while True:
        batch = _api_json(
            f"https://api.github.com/repos/{REPO}/stargazers"
            f"?per_page=100&page={page}",
            token, accept="application/vnd.github.star+json")
        if not batch:
            break
        times.extend(
            datetime.fromisoformat(s["starred_at"].replace("Z", "+00:00"))
            for s in batch if s.get("starred_at"))
        if len(batch) < 100:
            break
        page += 1
    times.sort()
    return times


def daily_series(times):
    """Collapse per-star timestamps to one cumulative sample per day."""
    series = []
    for i, t in enumerate(times):
        day = t.strftime("%Y-%m-%d")
        if series and series[-1][0] == day:
            series[-1][1] = i + 1
        else:
            series.append([day, i + 1])
    return series


def load_series(outdir):
    try:
        with open(os.path.join(outdir, SERIES_FILE)) as fh:
            return [[str(day), int(n)] for day, n in json.load(fh)]
    except FileNotFoundError:
        return []


def nice_step(span, target_ticks=4):
    raw = span / target_ticks
    mag = 10 ** math.floor(math.log10(raw))
    for m in (1, 2, 5, 10):
        if raw <= m * mag:
            return int(m * mag)
    return int(10 * mag)


def month_ticks(t0, t1, max_ticks=7):
    """First-of-month tick positions spanning [t0, t1]."""
    months = []
    y, m = t0.year, t0.month
    while (y, m) <= (t1.year, t1.month):
        months.append(datetime(y, m, 1, tzinfo=timezone.utc))
        m += 1
        if m == 13:
            y, m = y + 1, 1
    step = max(1, math.ceil(len(months) / max_ticks))
    return months[::step]


def render(points, mode):
    c = MODES[mode]
    t0, t1 = points[0][0], points[-1][0]
    total = points[-1][1]
    span_s = max((t1 - t0).total_seconds(), 1)

    def x(t):
        return M_LEFT + (W - M_LEFT - M_RIGHT) * (
            (t - t0).total_seconds() / span_s)

    y_max = max(total, 10)
    y_step = nice_step(y_max)
    y_top = y_step * math.ceil(y_max / y_step)

    def y(v):
        return H - M_BOT - (H - M_TOP - M_BOT) * (v / y_top)

    pts = [(x(t), y(n)) for t, n in points]
    # Thin the path: one point per pixel column is plenty.
    thinned, last_px = [], None
    for px, py in pts:
        if last_px is None or px - last_px >= 1:
            thinned.append((px, py))
            last_px = px
    if thinned[-1] != pts[-1]:
        thinned.append(pts[-1])
    path = "M" + " L".join(f"{px:.1f},{py:.1f}" for px, py in thinned)

    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" '
        f'font-family="-apple-system,BlinkMacSystemFont,\'Segoe UI\','
        f'Helvetica,Arial,sans-serif">',
        f'<rect width="{W}" height="{H}" rx="6" fill="{c["surface"]}"/>',
        f'<text x="{M_LEFT}" y="20" font-size="13" font-weight="600" '
        f'fill="{c["ink"]}">GitHub stars — {REPO}</text>',
    ]
    # Recessive horizontal grid + y labels.
    v = y_step
    while v <= y_top:
        gy = y(v)
        parts.append(f'<line x1="{M_LEFT}" y1="{gy:.1f}" x2="{W - M_RIGHT}" '
                     f'y2="{gy:.1f}" stroke="{c["grid"]}" stroke-width="1"/>')
        parts.append(f'<text x="{M_LEFT - 8}" y="{gy + 4:.1f}" font-size="11" '
                     f'text-anchor="end" fill="{c["ink2"]}">{v:,}</text>')
        v += y_step
    # Baseline + x labels on month boundaries.
    parts.append(f'<line x1="{M_LEFT}" y1="{H - M_BOT}" x2="{W - M_RIGHT}" '
                 f'y2="{H - M_BOT}" stroke="{c["grid"]}" stroke-width="1"/>')
    for t in month_ticks(t0, t1):
        if t < t0 or t > t1:
            continue
        label = t.strftime("%b %Y")
        parts.append(f'<text x="{x(t):.1f}" y="{H - M_BOT + 18}" '
                     f'font-size="11" text-anchor="middle" '
                     f'fill="{c["ink2"]}">{label}</text>')
    # The series: 2px line, one direct label at the end.
    parts.append(f'<path d="{path}" fill="none" stroke="{c["series"]}" '
                 f'stroke-width="2" stroke-linejoin="round"/>')
    ex, ey = pts[-1]
    parts.append(f'<circle cx="{ex:.1f}" cy="{ey:.1f}" r="4" '
                 f'fill="{c["series"]}"/>')
    parts.append(f'<text x="{ex + 10:.1f}" y="{ey + 4:.1f}" font-size="12" '
                 f'font-weight="600" fill="{c["ink"]}">{total:,}</text>')
    parts.append("</svg>")
    return "\n".join(parts)


def main():
    outdir = sys.argv[1] if len(sys.argv) > 1 else "."
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    os.makedirs(outdir, exist_ok=True)

    try:
        series = daily_series(fetch_star_times(token))
        source = "stargazer timestamps"
    except urllib.error.HTTPError as err:
        detail = err.read()[:200].decode("utf-8", "replace")
        print(f"stargazers listing unavailable (HTTP {err.code}: {detail}); "
              f"sampling stargazers_count instead", file=sys.stderr)
        count = _api_json(
            f"https://api.github.com/repos/{REPO}", token)["stargazers_count"]
        today = datetime.now(timezone.utc).strftime("%Y-%m-%d")
        series = [s for s in load_series(outdir) if s[0] < today]
        series.append([today, count])
        source = "stargazers_count sample"

    if not series:
        print("no stargazer data returned", file=sys.stderr)
        return 1
    with open(os.path.join(outdir, SERIES_FILE), "w") as fh:
        json.dump(series, fh)
        fh.write("\n")
    points = [(datetime.fromisoformat(day).replace(tzinfo=timezone.utc), n)
              for day, n in series]
    for mode in MODES:
        path = os.path.join(outdir, f"star-history-{mode}.svg")
        with open(path, "w") as fh:
            fh.write(render(points, mode))
        print(f"wrote {path} ({series[-1][1]:,} stars, from {source})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
