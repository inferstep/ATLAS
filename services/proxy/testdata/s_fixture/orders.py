import cache
import pricing

CUSTOMERS = [
    {"id": "c-1", "tier": "premium"},
    {"id": "c-2", "tier": "standard"},
]


def quote_all():
    out = []
    for c in CUSTOMERS:
        for sku in ("sku-1", "sku-2", "sku-3"):
            out.append((c["id"], sku, pricing.price_for(sku, c)))
    return out


def run():
    quote_all()
    quote_all()          # a second identical pass should be all hits
    return cache.stats()
