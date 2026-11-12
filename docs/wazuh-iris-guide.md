# Wazuh -> SOAR -> DFIR-IRIS: setup and first real test

This guide connects a Wazuh manager and a DFIR-IRIS 2.4 instance to SOAR:

```
Wazuh manager (10.10.192.205)                       SOAR (10.10.192.198, docker compose)
  rule level >= 5 --custom-soar, HTTP POST--> ingest :8081 -> queue -> processor
                                                     filters -> mapping -> grouping -> stored alert
                                                     trigger: rule.level > 7 -> workflow wazuh-to-iris
DFIR-IRIS (https://10.10.192.205:9443)  <--POST /alerts/add-- worker -> Python runner (iris.alert.create)
```

* Wazuh sends every alert of **level 5 and up**; SOAR stores all of them (search, cases, other workflows).
* Alerts **above level 7** become **IRIS alerts** (title, description, severity, the Wazuh alert as source
  content, IOCs, the agent as an asset, tags). Repeats of the same alert within 10 minutes are counted on the SOAR
  alert and do not create another IRIS alert.
* What you change on each machine is marked **[SOAR host]**, **[Wazuh manager]** or **[IRIS]**.

This setup is for a trusted network. Two shortcuts are on by choice and listed with their fix in
[Before production](#before-production): Wazuh -> SOAR is plain HTTP (the ingest token crosses the LAN in clear), and
SOAR does not verify IRIS's self-signed certificate (`verify_tls: false`).

---

## 1. Run SOAR [SOAR host]

### 1.1 Settings

```bash
cd /root/soar/WFsoar/soar
cp -n deploy/compose/.env.example deploy/compose/.env
openssl rand -hex 32          # -> the ingest token
openssl rand -base64 32       # -> the secret-encryption master key
```

Edit `deploy/compose/.env`:

```bash
# Wazuh pushes with this token (use the first value from above; never the default)
SOAR_SOURCE_TOKEN_WAZUH_PROD=<64 hex characters>
# publish ingest (8081) on the LAN address so the Wazuh manager can reach it; everything else stays on 127.0.0.1
SOAR_INGEST_PUBLISH=10.10.192.198
# encrypts stored secrets such as the IRIS API key. Set it BEFORE storing any secret and keep a copy:
# secrets stored with one key cannot be read with another.
SOAR_MASTER_KEY_ID=k1
SOAR_MASTER_KEY=<the base64 value>
# your admin API key for the commands below (soar_ + 32 or more random characters)
SOAR_BOOTSTRAP_API_KEY=soar_<random>
```

### 1.2 Start

```bash
make up          # builds the images (pulls the patched Go base), runs migrate and seed, starts everything
make ps          # every service healthy
```

`migrate` creates the indices this version needs (for example `soar-subscriptions`); `seed` installs the Wazuh
pack: the `wazuh-prod` push source, the `wazuh_alert@1` mapping, the `wazuh-default@1` filters and two lists.

### 1.3 Shell helpers

Paste into the terminal you work from (use your own values):

```bash
export SOAR_KEY='soar_<random>'                 # SOAR_BOOTSTRAP_API_KEY
export TOKEN='<64 hex characters>'              # SOAR_SOURCE_TOKEN_WAZUH_PROD
export API=http://localhost:8080/api/v1
api()  { curl -s -H "Authorization: Bearer $SOAR_KEY" -H 'Content-Type: application/json' -X "$1" "$API$2" "${@:3}"; }
push() { curl -s -w ' HTTP %{http_code}\n' -XPOST http://10.10.192.198:8081/ingest/wazuh-prod \
           -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
```

Checks:

```bash
api GET /me | jq -c .roles                                     # ["admin"]
api GET /sources | jq -r '.items[].name'                       # wazuh-prod, wazuh-pull
curl -s -o /dev/null -w '%{http_code}\n' http://10.10.192.198:8081/readyz     # 200
```

Then **from the Wazuh manager**: `curl -s -o /dev/null -w '%{http_code}\n' http://10.10.192.198:8081/readyz` must
print `200`. If it times out, open the port on the SOAR host:
`firewall-cmd --permanent --add-port=8081/tcp && firewall-cmd --reload`.

---

## 2. Send alerts from Wazuh [Wazuh manager]

### 2.1 Install the integration script

Copy the two files from the SOAR host's repository (`integrations/wazuh/push/`) to the manager:

```bash
# on the SOAR host
scp integrations/wazuh/push/custom-soar integrations/wazuh/push/custom-soar.py root@10.10.192.205:/tmp/
# on the Wazuh manager
install -o root -g wazuh -m 750 /tmp/custom-soar /tmp/custom-soar.py /var/ossec/integrations/
```

`custom-soar` is a small shell wrapper that runs `custom-soar.py` with Wazuh's own Python
(`/var/ossec/framework/python`). The name must start with `custom-` for Wazuh to accept it.

### 2.2 ossec.conf

Add inside `<ossec_config>` in `/var/ossec/etc/ossec.conf`:

```xml
<integration>
  <name>custom-soar</name>
  <hook_url>http://10.10.192.198:8081/ingest/wazuh-prod</hook_url>
  <api_key>PASTE SOAR_SOURCE_TOKEN_WAZUH_PROD HERE</api_key>
  <level>5</level>
  <alert_format>json</alert_format>
  <options>{"timeout": 5, "retries": 2}</options>
</integration>
```

| Element | Meaning |
|---|---|
| `hook_url` | SOAR ingest for the `wazuh-prod` source |
| `api_key` | the ingest token; SOAR answers 401 to anything else |
| `level` | only alerts of this level and above are sent |
| `alert_format` | must be `json` |
| `options` | `timeout` (s per try), `retries` (extra tries after a 429/5xx answer or a network error; Wazuh itself never retries), `backoff` (s, default 1), `ca_file` (for a later `https://` hook_url) |

Optional narrowing (Wazuh applies them before calling the script): `<rule_id>5712,5763</rule_id>`,
`<group>authentication_failed,</group>`, `<location>/var/log/secure</location>`. Several `<integration>` blocks
can point at the same script.

```bash
systemctl restart wazuh-manager
grep -i integrat /var/ossec/logs/ossec.log | tail -3      # "Enabling integration for: 'custom-soar'"
```

### 2.3 First alert without waiting for an attack

Run the script exactly as Wazuh does, with a sample level-10 alert:

```bash
cat > /tmp/soar-test.alert <<'EOF'
{"timestamp":"2026-11-12T10:00:00.000+0600","rule":{"level":10,"description":"sshd: brute force trying to get access to the system. Non existent user.","id":"5712","groups":["syslog","sshd","authentication_failures"],"mitre":{"id":["T1110"],"tactic":["Credential Access"],"technique":["Brute Force"]}},"agent":{"id":"001","name":"web01","ip":"10.10.192.50"},"manager":{"name":"wazuh-manager"},"id":"1731384000.1","decoder":{"name":"sshd"},"location":"/var/log/secure","full_log":"sshd[2211]: Invalid user admin from 203.0.113.7 port 51234","data":{"srcip":"203.0.113.7","srcuser":"admin"}}
EOF
sudo -u wazuh /var/ossec/integrations/custom-soar /tmp/soar-test.alert '<token>' http://10.10.192.198:8081/ingest/wazuh-prod debug
echo $?                                          # 0 = SOAR accepted it
tail -3 /var/ossec/logs/integrations.log        # "alert 1731384000.1 (rule 5712, level 10) accepted by SOAR"
```

From the SOAR host the same request is `push -d @/tmp/soar-test.alert` (copy the file over first).

The script writes failures to `/var/ossec/logs/integrations.log` (never the token). With `debug` (Wazuh adds it when
integrator debugging is on: `integrator.debug=2` in `/var/ossec/etc/local_internal_options.conf`) it logs every
delivery too.

A real alert: from another machine, fail SSH logins to an agent with a user that does not exist, eight or more
times within two minutes. Rule 5710 (level 5) fires for each attempt and rule 5712 (level 10, brute force) once
the frequency is reached.

### 2.4 See it in SOAR [SOAR host]

```bash
api GET '/alerts?source_id=wazuh-prod&limit=5' | jq '.items[] | {title, severity, occurrence_count, observables}'
api GET /queues/stats | jq .                    # backlog: should be near 0
api GET /dlq | jq '.items[:3]'                  # records that failed parsing or mapping, with the reason
```

---

## 3. Conditions, extraction and using values in SOAR

Every alert passes these stages in order; each one is a place to apply conditions:

| Stage | Where | Changes apply | Use it to |
|---|---|---|---|
| 1. Wazuh integration | `ossec.conf` `<level>`, `<rule_id>`, `<group>` | manager restart | coarse cut: what leaves Wazuh at all |
| 2. Filters | filter set `wazuh-default` + lists | seconds | drop noise, tag, change severity |
| 3. Mapping | `wazuh_alert` | seconds (new version) | extract fields and observables (IOCs) |
| 4. Grouping | mapping `dedup_key`, source `dedup.window` (10m) | seconds | count repeats instead of new alerts |
| 5. Workflow trigger | `triggers[].match` | seconds (publish) | decide which alerts start a workflow |
| 6. Inside the workflow | `inputs.from`, node `when`, `branch` | publish | decide what each step does and with which values |

All conditions are [CEL](https://github.com/google/cel-spec) expressions. Filters and the mapping see the Wazuh alert
as `raw`; triggers and workflows see the stored alert as `alert`, with the whole Wazuh alert under `alert.ext`.

### 3.1 Filters and lists (drop noise without a restart)

The default set drops rule ids in the list `wazuh_noisy_rule_ids`, decoders in `ignored_decoders`, alerts below level
3, and tags brute force or ransomware patterns `priority-pattern`. Lists change at once:

```bash
api PUT /lists/wazuh_noisy_rule_ids -d '{"items":["5501","5502"]}' | jq .items
# test any filter set against a sample before relying on it
api POST /filters/test -d "{\"filter_set\":\"wazuh-default@1\",\"sample\": $(cat /tmp/soar-test.alert)}" | jq '{drop, rule}'
```

Your own rule (for example: drop everything from lab agents, raise FIM alerts on `/etc`): create a new version of the
set, test it, then point the source at it.

```bash
api GET /filters/wazuh-default@1 | jq '{name, mode, default, rules}' > /tmp/fs.json
# edit /tmp/fs.json: add to "rules", for example
#   {"name":"drop-lab-agents","stage":"pre_map","action":"drop",
#    "when":"string(coalesce(get_path(raw, \"agent.name\"), \"\")).startsWith(\"lab-\")"}
#   {"name":"etc-changes-high","stage":"pre_map","action":"set_severity","severity":4,
#    "when":"string(coalesce(get_path(raw, \"syscheck.path\"), \"\")).startsWith(\"/etc/\")"}
api POST /filters --data-binary @/tmp/fs.json | jq -r '.id'          # -> wazuh-default@2
api POST /filters/test -d "{\"filter_set\":\"wazuh-default@2\",\"sample\": $(cat /tmp/soar-test.alert)}" | jq .
SID=$(api GET /sources | jq -r '.items[] | select(.name=="wazuh-prod") | .id')
api PATCH /sources/$SID -d '{"filters":["wazuh-default@2"]}' | jq .filters
```

Actions: `keep`, `drop`, `tag`, `set_severity`, `route` (start a named workflow). `mode: first_match` stops at the
first matching rule. Use `get_path(raw, "a.b")` and `coalesce(x, default)` for fields that may be missing.

### 3.2 Mapping: what is extracted

`wazuh_alert@1` turns a Wazuh alert into:

| SOAR alert field | From |
|---|---|
| `title` | `rule.description` |
| `description` | `full_log` |
| `severity` (1-5) | `rule.level`: 0-3 -> 1, 4-6 -> 2, 7-9 -> 3, 10-12 -> 4, 13+ -> 5 |
| `occurred_at` | `timestamp` |
| `tags` | `rule.groups` |
| `mitre` | `rule.mitre.id / technique / tactic` |
| `observables` | `agent.name` (host), `data.srcip` (ip, source), `data.srcuser` / `data.dstuser` (user), `syscheck.path` (file_path), `syscheck.sha256/sha1/md5_after` (hash) |
| `ext` | **the whole Wazuh alert**: every field is usable as `alert.ext.<path>` even if not mapped |

Observables are the IOCs SOAR sends to IRIS. To extract more (for example the destination IP, a URL, or every IP in
the log line), create `wazuh_alert@2`:

```bash
api GET /mappings/wazuh_alert@1 | jq '{name, kind, definition}' > /tmp/map.json
# edit /tmp/map.json: append to definition.observables, for example
#   {"type":"ip","value":"get_path(raw, \"data.dstip\")","role":"target"}
#   {"type":"url","value":"get_path(raw, \"data.url\")"}
#   {"type":"ip","from":"extract_ips(coalesce(get_path(raw, \"full_log\"), \"\"))"}     # "from" = a list
api POST /mappings/test -d "{\"definition\": $(jq .definition /tmp/map.json), \"sample\": $(cat /tmp/soar-test.alert)}" | jq '.alert.observables'
api POST /mappings --data-binary @/tmp/map.json | jq -r .id                              # -> wazuh_alert@2
api PATCH /sources/$SID -d '{"mapping":"wazuh_alert@2"}' | jq .mapping_id
```

Helpers available in mappings, filters and workflows: `get_path`, `coalesce`, `regex_extract(text, "re (group)")`,
`extract_ips`, `extract_urls`, `extract_emails`, `extract_hashes`, `lower`, `parse_time`, `level_map`,
`severity_map`, `is_empty_hash`, `in_list(value, "list_name")`, plus standard CEL (`in`, `matches`, `startsWith`,
`exists`, `filter`, `map`, `has`).

### 3.3 Workflow conditions

A trigger's `match` decides whether an alert starts the workflow. The shipped one:

```yaml
triggers:
  - type: alert
    match: 'alert.source_id in ["wazuh-prod", "wazuh-pull"] && int(alert.ext.rule.level) > 7'
```

Other conditions you can combine with `&&`, `||` and `!`:

| Condition | Expression |
|---|---|
| specific rules | `alert.ext.rule.id in ["5712", "5763", "550"]` |
| a rule group | `"authentication_failures" in alert.ext.rule.groups` |
| an agent name pattern | `alert.ext.agent.name.startsWith("web")` |
| a MITRE technique | `has(alert.mitre) && "T1110" in alert.mitre.technique_ids` |
| a public source IP | `alert.observables.exists(o, o.type == "ip" && !o.value.startsWith("10."))` |
| an optional field | `get_path(alert.ext, "data.srcip") != null` |
| text in the log | `has(alert.description) && alert.description.matches("(?i)invalid user")` |

Inside a workflow:

```yaml
inputs:
  - name: src_ip                                   # extracted once when the execution starts
    from: 'get_path(alert.ext, "data.srcip")'
    default: ""
nodes:
  - id: to_iris
    action: iris.alert.create
    when: 'inputs.src_ip != ""'                   # false skips this node, the flow continues
    params:
      title: '"Wazuh: " + alert.title'             # params are CEL: quote literal text
      iocs: '[{"type": "ip", "value": inputs.src_ip}]'
  - id: critical
    type: branch                                   # first matching case wins
    cases:
      - when: 'int(alert.ext.rule.level) >= 13'
        next: [open_case]
    default: [done]
  - id: open_case
    action: iris.case.create
    params: { name: '"Critical: " + alert.title', description: 'alert.description' }
  - { id: done, type: end }
edges: [to_iris -> critical]
```

A later node reads an earlier node's result as `steps.<id>.output...`, for example
`steps.to_iris.output.alert_id`, or `steps.open_case.output.case_id` with `iris.alert.merge_into_case`.

Change the shipped workflow (threshold, asset type, TLP) by editing `examples/workflows/wazuh_to_iris.yaml` and
publishing a new version; the newest published version of a name replaces the older one's triggers:

```bash
runner/python/.venv/bin/python - examples/workflows/wazuh_to_iris.yaml > /tmp/wf.json <<'EOF'
import json, sys, yaml
print(json.dumps({"definition": yaml.safe_load(open(sys.argv[1]))}))
EOF
NEW=$(api POST /workflows/$WF/versions --data-binary @/tmp/wf.json | jq -r .id); api POST /workflows/$NEW/publish | jq -r .status
```

Try a workflow without side effects: `api POST /workflows/$WF/run -d '{"dry_run": true, "alert_id": "<a stored alert id>"}'`
(write actions report what they would do).

---

## 4. Send to DFIR-IRIS

### 4.1 What SOAR calls

SOAR's `iris.alert.create` action (Python, in the runner) calls the IRIS 2.4 REST API with
`Authorization: Bearer <API key>`:

| Call | Why |
|---|---|
| `GET /api/ping` | connection test |
| `GET /manage/ioc-types/list`, `GET /manage/tlp/list`, `GET /manage/asset-type/list` | turn type names into this instance's ids (once per alert) |
| `POST /alerts/add` | create the alert |

The body SOAR sends for a level-10 SSH brute force (verified against a recording IRIS stand-in):

```json
{
  "alert_title": "Wazuh: sshd: brute force trying to get access to the system. Non existent user.",
  "alert_description": "Wazuh rule 5712 (level 10) on agent web01 (001)\n\nsshd[2211]: Invalid user admin from 203.0.113.7 port 51234",
  "alert_source": "Wazuh", "alert_source_ref": "1731384000.1",
  "alert_severity_id": 5, "alert_status_id": 2, "alert_customer_id": 1,
  "alert_source_event_time": "2026-11-12T04:00:00Z",
  "alert_source_content": { "...": "the whole Wazuh alert" },
  "alert_tags": "wazuh,syslog,sshd,authentication_failures,priority-pattern,T1110",
  "alert_note": "SOAR alert alt_...",
  "alert_iocs": [
    {"ioc_value": "203.0.113.7", "ioc_type_id": 76, "ioc_tlp_id": 2, "ioc_description": "Wazuh ip (source)", "ioc_tags": ""},
    {"ioc_value": "admin", "ioc_type_id": 3, "ioc_tlp_id": 2, "ioc_description": "Wazuh user (source)", "ioc_tags": ""}
  ],
  "alert_assets": [
    {"asset_name": "web01", "asset_type_id": 3, "asset_ip": "10.10.192.50", "asset_description": "Wazuh agent 001", "asset_tags": ""}
  ]
}
```

| IRIS field | Value |
|---|---|
| severity | SOAR severity 3 -> IRIS severity id 4, 4 -> 5, 5 -> 6 (Medium, High, Critical on a default IRIS install; check the names an alert shows on yours) |
| status | 2 (New) |
| IOC types | ip -> `ip-any`, user -> `account`, file_path -> `filename`, hash -> `md5` / `sha1` / `sha256` by length |
| asset type | `Linux - Server` (workflow input `asset_type`) |
| TLP | `amber` (workflow input `tlp`) |

An IOC or asset type your IRIS does not have is skipped and listed in the step output (`skipped`); the alert is
still created. Other actions in the same pack: `iris.case.create/update/close`, `iris.alert.update`,
`iris.alert.merge_into_case`, `iris.ioc.add`, `iris.asset.add`, `iris.note.add`, `iris.timeline.event.add`,
`iris.task.create` (paths in `integrations/dfir_iris/actions/endpoints.yaml`).

### 4.2 Prepare IRIS [IRIS]

1. A service account (IRIS admin menu, access control) with permission to write alerts and access to the customer
   SOAR writes to. Its API key: log in as that user, open *My settings*, copy the API key.
2. The customer id (admin menu, customers); the first customer, *IrisInitialClient*, is 1.
3. Check the API and the type names from the SOAR host, with the same calls SOAR makes:
   ```bash
   read -rs IRIS_KEY                                     # paste the key; it stays out of the shell history
   IRIS=https://10.10.192.205:9443
   curl -sk -H "Authorization: Bearer $IRIS_KEY" $IRIS/api/ping                  # "status": "success"
   curl -sk -H "Authorization: Bearer $IRIS_KEY" $IRIS/manage/ioc-types/list  | jq -r '.data[].type_name'  | grep -xE 'ip-any|account|filename|md5|sha1|sha256'
   curl -sk -H "Authorization: Bearer $IRIS_KEY" $IRIS/manage/asset-type/list | jq -r '.data[].asset_name' | grep -x 'Linux - Server'
   curl -sk -H "Authorization: Bearer $IRIS_KEY" $IRIS/manage/tlp/list        | jq -r '.data[].tlp_name'   | grep -x amber
   ```
   A name missing from the output means that IOC or asset type will be skipped; use names your instance has in the
   workflow inputs (`asset_type`, `tlp`) or add them in IRIS.

### 4.3 Connect SOAR to IRIS [SOAR host]

```bash
# the API key as an encrypted SOAR secret, released only to IRIS connections
api PUT /secrets/iris_api_key -d "{\"value\":\"$IRIS_KEY\",\"scope\":\"dfir_iris\"}" | jq '{name, scope}'
unset IRIS_KEY

CID=$(api POST /connections -d '{
  "connector_id": "dfir_iris", "name": "iris", "auth_type": "bearer",
  "secret_refs": {"api_key": "iris_api_key"},
  "config": {"base_url": "https://10.10.192.205:9443", "customer_id": 1, "verify_tls": false}
}' | jq -r .id); echo $CID
api POST /connections/$CID/test | jq '{ok, error}'      # {"ok": true}
```

| Test result | Meaning |
|---|---|
| `ok: true` | SOAR reached IRIS with the key |
| `http_401` / `http_403` | wrong key, or the user lacks permission |
| `network ... CERTIFICATE_VERIFY_FAILED` | `verify_tls` is not `false` and IRIS's certificate is self-signed |
| `network ... timed out` / `refused` | port 9443 not reachable from the SOAR host (firewall, IRIS down) |
| `missing_secret` | the secret name or its scope does not match the connection |

`secret_refs` names the secret (no `secret:` prefix). Do not scope this secret to `host:10.10.192.205`: with
`verify_tls: false` SOAR refuses host-pinned secrets (the host cannot be proven).

### 4.4 Install the workflow [SOAR host]

```bash
make runner-venv            # once: Python with PyYAML for the converter below
runner/python/.venv/bin/python - examples/workflows/wazuh_to_iris.yaml > /tmp/wf.json <<'EOF'
import json, sys, yaml
print(json.dumps({"name": "wazuh-to-iris", "definition": yaml.safe_load(open(sys.argv[1]))}))
EOF
WF=$(api POST /workflows --data-binary @/tmp/wf.json | jq -r .id)
api POST /workflows/$WF/publish | jq -r .status          # published
sleep 12                                                  # processors reload triggers every 10 s
```

### 4.5 End-to-end test

1. On the Wazuh manager, send the sample alert (2.3) again with a new `"id"`, or cause a real level 8+ alert.
2. In SOAR:
   ```bash
   E=$(api GET "/executions?workflow_id=$WF&limit=1" | jq -r '.items[0].id')
   api GET /executions/$E | jq '{status, iris: .context.nodes.to_iris.output, error: .context.nodes.to_iris.error}'
   ```
   `status: succeeded` and `iris.alert_id` = the new IRIS alert id.
3. In IRIS: *Alerts* lists "Wazuh: ..." with its IOCs, the agent asset and the Wazuh alert under source content.
4. A level 5-7 alert appears in SOAR (`api GET /alerts`) and starts no execution.

### 4.6 When something does not arrive

| Symptom | Look at |
|---|---|
| nothing in SOAR | manager: `/var/ossec/logs/integrations.log` (`NOT delivered ... HTTP 401` = token; `unreachable` = network or `SOAR_INGEST_PUBLISH`), `ossec.log` for integratord errors |
| SOAR answers 503 | the queue is full or starting: `api GET /queues/stats`; the script retries twice |
| alert missing but 202 returned | `api GET /dlq` (parse or mapping failures), filters (`/filters/test` with that alert) |
| alert stored, no execution | the trigger: level, `source_id`, workflow published, 10 s reload; `api GET /workflows/$WF \| jq .status` |
| execution failed | `api GET /executions/$E/steps \| jq '.items[] \| {node_id, status, error}'`; worker log `make logs S=worker` |
| `iris_error` | IRIS refused the body: the message says which field (customer id, permissions) |
| IOC or asset missing in IRIS | step output `skipped`: the type name does not exist in your IRIS (4.2 step 3) |

---

## Before production

* **HTTPS from Wazuh to SOAR.** Give the api/ingest a certificate (`SOAR_TLS_CERT_FILE`, `SOAR_TLS_KEY_FILE`, see
  docs in the README, Step 9), change `hook_url` to `https://`, copy the CA to the manager and set
  `"ca_file": "/var/ossec/etc/soar-ca.pem"` in `<options>`.
* **Verify IRIS's certificate.** Reissue IRIS's certificate with `10.10.192.205` as an IP subject alternative name,
  put its CA in the connection's `ca_pem`, remove `verify_tls` (default true). The secret can then be scoped to
  `host:10.10.192.205`.
* Replace every development default in `.env` (master key, bootstrap key, S3 and Keycloak passwords), and set up
  OIDC users instead of the bootstrap key (README, Step 9).
