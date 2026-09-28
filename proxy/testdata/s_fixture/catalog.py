PRODUCTS = {
    "sku-1": {"name": "Widget", "base": 10.0, "tier": "standard"},
    "sku-2": {"name": "Gadget", "base": 25.0, "tier": "premium"},
    "sku-3": {"name": "Doohickey", "base": 99.0, "tier": "premium"},
}


def get(sku):
    return PRODUCTS[sku]
