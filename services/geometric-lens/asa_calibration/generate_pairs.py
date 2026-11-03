#!/usr/bin/env python3
"""Generate ~1000 structural_edit-vs-edit_file contrast pairs for ASA steering
vector extraction.

Pairs are emitted in positional-symmetric order so cvector-generator
contrasts line N (structural_edit positive) against line N+1 (edit_file
negative) on the SAME user task — which is what isolates the tool-
choice direction in residual space (task-comprehension noise cancels).

Diversity strategy: a small number of richly-templated base scenarios
combined with deep variation pools (file paths, function/class/element
names, decorator stacks, task verbs, prose framings) gives genuine
combinatorial coverage rather than mechanical renames.

Usage:
    python generate_pairs.py --out contrast_pairs.jsonl --n 1000 --seed 42
"""

from __future__ import annotations

import argparse
import json
import random
from pathlib import Path


# ---------------------------------------------------------------------------
# Variation pools
# ---------------------------------------------------------------------------

PY_FILES = [
    "app.py", "main.py", "server.py", "api.py", "views.py", "handlers.py",
    "routes.py", "models.py", "schemas.py", "db.py", "auth.py", "billing.py",
    "users.py", "orders.py", "products.py", "cart.py", "checkout.py",
    "search.py", "uploads.py", "tasks.py", "jobs.py", "workers.py",
    "pipeline.py", "ingest.py", "etl.py", "transform.py", "validators.py",
    "utils/cache.py", "utils/rate_limit.py", "utils/logging_config.py",
    "services/email.py", "services/payment.py", "services/notify.py",
    "services/storage.py", "services/auth.py", "services/search.py",
    "blueprints/users.py", "blueprints/admin.py", "blueprints/api.py",
    "routes/v1/users.py", "routes/v1/orders.py", "routes/v2/products.py",
    "core/config.py", "core/security.py", "core/middleware.py",
    "domain/order.py", "domain/user.py", "domain/invoice.py",
    "infra/queue.py", "infra/cache.py", "infra/db_session.py",
    "cli.py", "manage.py", "scripts/migrate.py", "scripts/seed.py",
    "tests/conftest.py", "tests/fixtures.py",
    "graphql/resolvers.py", "graphql/mutations.py",
    "ml/preprocess.py", "ml/inference.py", "ml/training.py",
]

HTML_FILES = [
    "templates/index.html", "templates/dashboard.html", "templates/login.html",
    "templates/signup.html", "templates/profile.html", "templates/settings.html",
    "templates/base.html", "templates/layout.html", "templates/404.html",
    "templates/500.html", "templates/admin/users.html", "templates/admin/orders.html",
    "templates/admin/dashboard.html", "templates/products/list.html",
    "templates/products/detail.html", "templates/cart/view.html",
    "templates/checkout/payment.html", "templates/checkout/confirm.html",
    "templates/auth/forgot.html", "templates/auth/reset.html",
    "templates/blog/post.html", "templates/blog/list.html",
    "templates/email/welcome.html", "templates/email/receipt.html",
    "templates/partials/navbar.html", "templates/partials/footer.html",
    "static/landing.html", "static/about.html", "static/pricing.html",
    "static/contact.html", "docs/index.html", "docs/api.html",
]

FUNCTION_NAMES = [
    # routes / handlers
    "dashboard", "login", "logout", "register", "profile", "settings",
    "search", "checkout", "cart_view", "cart_add", "cart_remove",
    "list_products", "get_product", "create_product", "update_product",
    "delete_product", "list_orders", "get_order", "place_order",
    "cancel_order", "refund_order", "process_payment", "verify_payment",
    "send_receipt", "forgot_password", "reset_password", "verify_email",
    # utilities
    "parse_args", "parse_config", "load_settings", "validate_input",
    "sanitize_html", "format_currency", "format_date", "compute_tax",
    "generate_invoice", "compute_total", "apply_discount", "round_money",
    # services
    "send_email", "send_sms", "publish_event", "consume_message",
    "enqueue_job", "process_job", "retry_failed", "schedule_task",
    # data
    "fetch_orders", "stream_records", "batch_insert", "upsert_user",
    "build_query", "execute_query", "paginate_results",
    # ml
    "preprocess_features", "tokenize_text", "embed_documents",
    "score_candidates", "rank_results",
    # async / generators
    "stream_events", "watch_directory", "tail_logs", "walk_tree",
    "iter_pages", "yield_batches",
]

CLASS_NAMES = [
    # ORM / models
    "User", "UserModel", "Order", "OrderModel", "Product", "ProductModel",
    "Cart", "CartItem", "Invoice", "Payment", "Subscription", "Plan",
    "Address", "Customer", "Vendor", "Inventory",
    # Pydantic schemas
    "UserCreate", "UserUpdate", "UserResponse", "OrderCreate", "OrderUpdate",
    "ProductCreate", "ProductFilter", "PaymentRequest", "RefundRequest",
    # plain
    "Cache", "RateLimiter", "EventBus", "RetryPolicy", "CircuitBreaker",
    "ConnectionPool", "TaskQueue", "Scheduler", "Logger", "MetricsCollector",
    # enum / exception
    "OrderStatus", "PaymentStatus", "UserRole", "Permission",
    "ApiError", "ValidationError", "AuthError", "RateLimitError",
    # dataclass
    "OrderItem", "ShippingInfo", "BillingInfo", "TaxRate", "Discount",
]

HTML_ELEMENTS = [
    "<body>", "<head>", "<header>", "<nav>", "<main>", "<footer>",
    "<form>", "<aside>", "<section>", "<article>",
]

DECORATORS_PLAIN = [
    "@app.route('/dashboard')", "@app.route('/login', methods=['POST'])",
    "@bp.route('/users/<int:id>')", "@router.get('/orders')",
    "@router.post('/checkout')", "@app.get('/health')", "@app.post('/webhook')",
    "@login_required", "@admin_required", "@cache.memoize(300)",
    "@retry(tries=3)", "@contextmanager", "@dataclass", "@property",
    "@classmethod", "@staticmethod", "@pytest.fixture",
    "@pytest.mark.parametrize('x', [1, 2, 3])",
    "@app.errorhandler(404)", "@celery.task",
]

DECORATOR_STACKS = [
    "@app.route('/admin/users')\\n@login_required\\n@admin_required",
    "@bp.route('/api/orders')\\n@require_json\\n@validate_request",
    "@router.post('/upload')\\n@rate_limit('10/minute')",
    "@cache.memoize(60)\\n@require_auth",
    "@celery.task(bind=True, max_retries=3)",
]

TASK_VERBS = [
    "Update", "Replace", "Rewrite", "Refactor", "Modify", "Change",
    "Convert", "Restructure", "Reimplement", "Overhaul",
]

PY_GOALS = [
    "return JSON instead of an HTML template",
    "validate the input payload via Pydantic",
    "add structured logging with the user_id",
    "use the new database session helper",
    "enforce a per-user rate limit",
    "support pagination via offset/limit query params",
    "include CSRF protection on the POST handler",
    "swap the synchronous DB call for an async one",
    "batch downstream API calls in groups of 25",
    "return a 410 Gone response for soft-deleted records",
    "emit a Prometheus counter on each invocation",
    "wrap the body in a transaction with rollback on exception",
    "fall back to the cache when the primary store is down",
    "add a retry loop with exponential backoff",
    "honor the X-Request-ID header for tracing",
    "skip records older than 30 days",
    "use the new payment provider's SDK",
    "stream the response instead of buffering",
    "redirect non-admins to /home",
    "validate password length before hashing",
    "include the discount field in the response",
    "be thread-safe with a threading.Lock",
    "raise a typed exception on bad input",
    "support an optional --verbose flag",
    "log the elapsed_ms for each call",
    "use the new schema with the email_verified column",
    "compute the tax based on the shipping address",
    "expire entries after 5 minutes",
    "add a circuit breaker around the upstream call",
    "use yield to stream results lazily",
]

HTML_GOALS = [
    "add a sticky navigation bar with a search box",
    "use a hamburger menu on mobile",
    "include a copyright + social links footer",
    "add a CSRF token + remember-me checkbox",
    "use a two-column layout with sidebar",
    "add Tailwind CDN + a custom favicon link",
    "show a paginated table",
    "add hero section with CTA button",
    "use a dark-mode-aware color scheme",
    "include breadcrumb navigation",
    "add a cookie-consent banner",
    "show a loading skeleton while data fetches",
    "include the user avatar dropdown menu",
    "add an empty-state illustration with a call to action",
    "include the cart icon with item count badge",
    "use semantic HTML5 sectioning",
    "add ARIA labels for accessibility",
    "include OpenGraph meta tags",
    "use HTMX for inline edits",
    "add Alpine.js for the dropdown menu",
    "include the language switcher",
    "show recent activity timeline",
    "use a progressive disclosure pattern for advanced options",
    "include a feedback widget",
    "add print-only styles in the head",
]

PROSE_STRUCTURAL_EDIT = [
    "I'll rewrite {name} with structural_edit using selector {selector} — that's a whole-{kind} swap and structural_edit is the right tool.",
    "Switching to structural_edit selector {selector} since this is a whole-{kind} rewrite.",
    "I'll use structural_edit on {file} with selector {selector} — single structural-{kind} swap.",
    "structural_edit selector {selector} is the cleanest call here — no need to copy the existing {kind} body as old_str.",
    "Going to use structural_edit with selector {selector} — whole-{kind} replacement.",
    "Let me use structural_edit selector {selector} — exactly the case structural_edit was designed for (named-node rewrite).",
    "I should use structural_edit with selector {selector} since this is a complete {kind} rewrite, not a surgical line edit.",
    "I'll use structural_edit on {file} (selector {selector}) — whole-{kind} swap, no truncation risk on long content.",
    "structural_edit with selector {selector} fits — the user is asking for a {kind}-level rewrite.",
    "I'll rewrite the {kind} with structural_edit selector {selector}; decorators and signatures are pulled in automatically.",
    "Using structural_edit on {file} with selector {selector} — whole-block swap is the right shape for this change.",
    "I'll go with structural_edit selector {selector} — replacing the entire {kind} cleanly without copying it as old_str.",
    "Picking structural_edit selector {selector} since the change touches the whole {kind}, not just a line or two.",
    "structural_edit on {file} with selector {selector} — that's the structural-edit path for whole-{kind} swaps.",
    "I'll use structural_edit (selector {selector}) — old_str-based edit_file would force me to copy the entire existing {kind}.",
]

PROSE_EDIT_FILE = [
    "I'll use edit_file with the entire {kind} as old_str and the new version as new_str.",
    "I'll use edit_file to replace the entire content of the {kind} in {file} with the new version.",
    "Going to use edit_file with old_str = the existing {kind} body and new_str = the rewritten version.",
    "I'll edit_file the whole {kind} block — old_str is the existing {kind}, new_str is the updated one.",
    "Use edit_file with the {kind} body as old_str and the new code as new_str.",
    "I'll use edit_file to swap the entire {kind} block with the new implementation.",
    "Let me use edit_file — old_str will be the entire existing {kind}, new_str will be the new version.",
    "I'll go with edit_file: old_str is the existing {kind}, new_str is the refactored version.",
    "edit_file is the right call — set old_str to the current {kind} and new_str to the rewrite.",
    "I'll use edit_file with old_str matching the entire {kind} and new_str containing the rewrite.",
    "Going with edit_file — copy the existing {kind} into old_str and the new version into new_str.",
    "I'll edit_file the {kind} block by setting old_str to the current code and new_str to the new code.",
    "Use edit_file: old_str is the existing {kind}, new_str is the version with the change.",
    "I'll use edit_file to replace the {kind} block — the entire existing {kind} as old_str, the new one as new_str.",
    "edit_file with the {kind} as old_str and the rewritten {kind} as new_str.",
]

REJECTION_PREAMBLES = [
    "The previous write_file on {file} was rejected because the file already exists. ",
    "write_file got rejected for {file} (already exists). ",
    "Need to update {file} but write_file failed since the file is already on disk. ",
    "{file} already exists so write_file isn't allowed. ",
    "Got the existing-file rejection on {file}. ",
]


# ---------------------------------------------------------------------------
# Template definitions — each yields a (user, ast_prefix, edit_prefix) triple
# ---------------------------------------------------------------------------

def template_function(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(FUNCTION_NAMES)
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(PY_GOALS)
    selector = f"function:{name}"
    user = f"{verb} the {name} function in {file} to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name=name, selector=selector, kind="function", file=file)
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="function", file=file)
    return user, pos, neg


def template_decorated_function(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(FUNCTION_NAMES)
    decorator = rng.choice(DECORATORS_PLAIN)
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(PY_GOALS)
    selector = f"function:{name}"
    user = f"{verb} the {decorator} {name} handler in {file} to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name=name, selector=selector, kind="function", file=file) + " The decorator comes along automatically."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="function", file=file)
    return user, pos, neg


def template_stacked_decorator_function(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(FUNCTION_NAMES)
    stack = rng.choice(DECORATOR_STACKS).replace("\\n", " + ")
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(PY_GOALS)
    selector = f"function:{name}"
    user = f"{verb} the {name} handler in {file} (stacked decorators: {stack}) to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name=name, selector=selector, kind="function", file=file) + " The full decorator stack is preserved by structural_edit."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="function", file=file)
    return user, pos, neg


def template_async_function(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(FUNCTION_NAMES)
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(PY_GOALS)
    selector = f"function:{name}"
    user = f"{verb} the async {name} function in {file} to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name=name, selector=selector, kind="function", file=file) + " async signatures are handled the same as sync."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="function", file=file)
    return user, pos, neg


def template_generator_function(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(FUNCTION_NAMES)
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(PY_GOALS)
    selector = f"function:{name}"
    user = f"{verb} the {name} generator in {file} to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name=name, selector=selector, kind="function", file=file) + " A yield-based generator is just a function for the AST."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="function", file=file)
    return user, pos, neg


def template_class(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(CLASS_NAMES)
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(PY_GOALS)
    selector = f"class:{name}"
    user = f"{verb} the {name} class in {file} to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name=name, selector=selector, kind="class", file=file)
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="class", file=file)
    return user, pos, neg


def template_decorated_class(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(CLASS_NAMES)
    decorator = rng.choice(["@dataclass", "@dataclass(frozen=True)",
                            "@final", "@attrs.define"])
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(PY_GOALS)
    selector = f"class:{name}"
    user = f"{verb} the {decorator} {name} class in {file} to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name=name, selector=selector, kind="class", file=file) + " The class decorator is captured automatically."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="class", file=file)
    return user, pos, neg


def template_html_element(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(HTML_FILES)
    element = rng.choice(HTML_ELEMENTS)
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(HTML_GOALS)
    selector = element
    elem_name = element.strip("<>")
    user = f"{verb} the {element} element in {file} to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name=elem_name, selector=selector, kind="element", file=file)
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=elem_name, selector=selector, kind="element", file=file)
    return user, pos, neg


def template_html_body_redesign(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(HTML_FILES)
    verb = rng.choice(TASK_VERBS)
    goal = rng.choice(HTML_GOALS)
    selector = "<body>"
    user = f"{verb} the body of {file} to {goal}."
    pos = rng.choice(PROSE_STRUCTURAL_EDIT).format(
        name="body", selector=selector, kind="element", file=file)
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name="body", selector=selector, kind="element", file=file)
    return user, pos, neg


def template_post_rejection_function(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(FUNCTION_NAMES)
    goal = rng.choice(PY_GOALS)
    selector = f"function:{name}"
    preamble = rng.choice(REJECTION_PREAMBLES).format(file=file)
    user = preamble + f"The {name} function needs to {goal} — replace it."
    pos = "Switching to structural_edit selector " + selector + \
          " — whole-function swap is the right tool for an existing-file rewrite, edit_file would force me to copy the whole function body."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="function", file=file)
    return user, pos, neg


def template_post_rejection_class(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(PY_FILES)
    name = rng.choice(CLASS_NAMES)
    goal = rng.choice(PY_GOALS)
    selector = f"class:{name}"
    preamble = rng.choice(REJECTION_PREAMBLES).format(file=file)
    user = preamble + f"The {name} class needs to {goal} — refactor it."
    pos = "Switching to structural_edit selector " + selector + \
          " — whole-class swap is the right tool for an existing-file rewrite."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="class", file=file)
    return user, pos, neg


def template_post_rejection_html(rng: random.Random) -> tuple[str, str, str]:
    file = rng.choice(HTML_FILES)
    element = rng.choice(HTML_ELEMENTS)
    goal = rng.choice(HTML_GOALS)
    selector = element
    elem_name = element.strip("<>")
    preamble = rng.choice(REJECTION_PREAMBLES).format(file=file)
    user = preamble + f"Need to swap the {element} so it can {goal} — the whole element changes."
    pos = "Switching to structural_edit with selector " + selector + " on " + file + \
          " — whole-element swap, doesn't truncate on long content the way edit_file can."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=elem_name, selector=selector, kind="element", file=file)
    return user, pos, neg


def template_entire_keyword(rng: random.Random) -> tuple[str, str, str]:
    """User explicitly says 'entire' — strong structural_edit signal."""
    file = rng.choice(PY_FILES)
    name = rng.choice(FUNCTION_NAMES)
    verb = rng.choice(["Refactor", "Rewrite", "Replace"])
    goal = rng.choice(PY_GOALS)
    selector = f"function:{name}"
    user = f"{verb} the entire {name} function in {file} to {goal}."
    pos = "I'll use structural_edit selector " + selector + \
          " — the user said 'entire function', that's a whole-node rewrite which is structural_edit's specialty."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name, selector=selector, kind="function", file=file)
    return user, pos, neg


def template_swap_keyword(rng: random.Random) -> tuple[str, str, str]:
    """User says 'swap' — strong structural signal."""
    file = rng.choice(HTML_FILES) if rng.random() < 0.4 else rng.choice(PY_FILES)
    if file.endswith(".html"):
        element = rng.choice(HTML_ELEMENTS)
        selector = element
        elem_name = element.strip("<>")
        goal = rng.choice(HTML_GOALS)
        user = f"Swap the {element} in {file} for one that can {goal}."
        kind = "element"
        name_for_prose = elem_name
    else:
        name = rng.choice(FUNCTION_NAMES)
        selector = f"function:{name}"
        goal = rng.choice(PY_GOALS)
        user = f"Swap the {name} function in {file} for a version that can {goal}."
        kind = "function"
        name_for_prose = name
    pos = "structural_edit with selector " + selector + \
          " — 'swap' is the structural keyword, exactly what structural_edit handles."
    neg = rng.choice(PROSE_EDIT_FILE).format(
        name=name_for_prose, selector=selector, kind=kind, file=file)
    return user, pos, neg


# Weighted template registry — controls the mix in the final output.
TEMPLATES = [
    (template_function, 18),                # plain functions: most common
    (template_decorated_function, 12),
    (template_stacked_decorator_function, 4),
    (template_async_function, 6),
    (template_generator_function, 4),
    (template_class, 12),
    (template_decorated_class, 4),
    (template_html_element, 14),
    (template_html_body_redesign, 6),
    (template_post_rejection_function, 6),
    (template_post_rejection_class, 4),
    (template_post_rejection_html, 4),
    (template_entire_keyword, 3),
    (template_swap_keyword, 3),
]


def weighted_pick(rng: random.Random) -> callable:
    total = sum(w for _, w in TEMPLATES)
    r = rng.uniform(0, total)
    upto = 0.0
    for fn, w in TEMPLATES:
        upto += w
        if r <= upto:
            return fn
    return TEMPLATES[-1][0]


# ---------------------------------------------------------------------------
# Generation
# ---------------------------------------------------------------------------

def generate(n: int, seed: int) -> list[dict]:
    rng = random.Random(seed)
    seen_users: set[str] = set()
    pairs: list[dict] = []
    # Try ~3x oversampling and dedupe by user text — the variation pools
    # have enough cardinality (~10^6 combinations) that 3x is plenty to
    # hit n unique pairs without the loop running forever.
    attempts = 0
    max_attempts = n * 8
    while len(pairs) // 2 < n and attempts < max_attempts:
        attempts += 1
        fn = weighted_pick(rng)
        user, pos_prefix, neg_prefix = fn(rng)
        if user in seen_users:
            continue
        seen_users.add(user)
        pairs.append({"label": "structural_edit", "user": user,
                      "assistant_prefix": pos_prefix, "tool": "structural_edit"})
        pairs.append({"label": "edit_file", "user": user,
                      "assistant_prefix": neg_prefix, "tool": "edit_file"})
    if len(pairs) // 2 < n:
        raise RuntimeError(f"only generated {len(pairs)//2} unique pairs after "
                           f"{attempts} attempts — variation pools too small for n={n}")
    return pairs


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, type=Path,
                    help="output JSONL path (overwritten)")
    ap.add_argument("--n", type=int, default=1000,
                    help="number of contrast PAIRS (each pair = 2 lines)")
    ap.add_argument("--seed", type=int, default=42,
                    help="RNG seed for reproducibility")
    args = ap.parse_args()

    pairs = generate(args.n, args.seed)
    with args.out.open("w") as f:
        for p in pairs:
            f.write(json.dumps(p) + "\n")
    print(f"wrote {len(pairs)} lines ({len(pairs)//2} contrast pairs) to {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
