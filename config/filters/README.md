Script filters for ingestion (spec 9.5.1). A filter rule `{script: filters/<name>.py}` runs
`decide(events) -> list[str]` in the sandboxed runner with batches of up to 256 events and returns
one decision per event: `keep`, `drop`, `tag:<name>` or `severity:<1-5>`.
