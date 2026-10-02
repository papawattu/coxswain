"""Small numeric helpers (stdlib only)."""


def mean(values):
    """Arithmetic mean; raises ValueError on an empty sequence."""
    if not values:
        raise ValueError("mean of empty sequence")
    return sum(values) / len(values)


def mode(values):
    """Most frequent value; ties resolve to the earliest seen."""
    if not values:
        raise ValueError("mode of empty sequence")
    best, best_count = None, 0
    for v in values:
        c = values.count(v)
        if c > best_count:
            best, best_count = v, c
    return best
