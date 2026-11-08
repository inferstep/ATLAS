"""Cache key construction."""


def write_key(sku, customer):
    """Key used when a computed price is stored."""
    return "%s:%s" % (sku, customer["id"])


def read_key(sku, customer):
    """Key used when a price is looked up."""
    return "%s:%s" % (sku, customer["tier"])
