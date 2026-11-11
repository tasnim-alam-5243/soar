# Wazuh source pack

* `mappings/`, `filters/`, `sources/`, `lists/`: installed by `soar seed` (idempotent; never overwrites edits).
* `push/`: the Integrator script to copy to the Wazuh manager (see below).
* `testdata/`: golden fixtures used by CI. **These six files were constructed from the field descriptions in
  the platform spec, not captured from a live Wazuh 4.14.** Replace them with real samples (same file names,
  same `rule.id`s) and run `go test ./integrations/... -update` to regenerate the `.expected.json` files, then
  review the diff: that is how real payloads are verified before the pack is marked supported.

## Push (v1)

On the Wazuh manager:

```bash
install -o root -g wazuh -m 750 push/custom-soar push/custom-soar.py /var/ossec/integrations/
```

```xml
<integration>
  <name>custom-soar</name>
  <hook_url>https://SOAR_HOST:8081/ingest/wazuh-prod</hook_url>
  <level>3</level>   <!-- coarse; fine filtering happens in SOAR -->
  <alert_format>json</alert_format>
  <api_key>VALUE OF SOAR_SOURCE_TOKEN_WAZUH_PROD</api_key>
</integration>
```

Changing integration settings in Wazuh needs a manager restart; SOAR filter changes apply within seconds.

## Pull (v1)

Create a read-only indexer user, then set `SOAR_WAZUH_INDEXER_URL`, `SOAR_WAZUH_INDEXER_USER`,
`SOAR_WAZUH_INDEXER_PASSWORD` (and `SOAR_WAZUH_INDEXER_CA_FILE`) in `deploy/compose/.env` and run `make seed`.
Push and pull use the same `system: wazuh-prod` and the Wazuh alert `id` as `external_id`, so an alert
that arrives both ways is stored once.
