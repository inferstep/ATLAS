"""Price computation with caching."""

import cache
import catalog
import keys


def _compute(sku, customer):
    product = catalog.get(sku)
    price = product["base"]
    if customer["tier"] == "premium":
        price *= 0.9
    return round(price, 2)


def price_for(sku, customer):
    hit = cache.get(keys.read_key(sku, customer))
    if hit is not None:
        return hit
    value = _compute(sku, customer)
    cache.put(keys.write_key(sku, customer), value)
    return value
