def decide(events):
    return ["drop" if e.get("n", 0) % 2 else "keep" for e in events]
