# NER plugin smoke (`gatehouse serve` + astrlink-detector)

Run 2026-11-25 00:54. Config (paths shortened):

```yaml
listen: <listen>
mode: personal
providers:
  - name: cloud
    base_url: <fake-upstream>/v1
    target: external
    models: [strong]
plugins:
  - name: astrlink
    kind: detector
    path: <plugin>
    args: ["-model", "<model>", "-ort", "<ort>"]
redaction:
  mode_by_target: { local: audit, external: mask }
  detectors: [astrlink]
  categories: { person: mask, address: mask, account: mask, ner_secret: mask, date: mask, ner_card: mask, secret: block }
  fail_closed: true
```

- **PASS** gateway healthy with plugin — startup (incl. plugin handshake and model load) 616 ms
- **PASS** PII request accepted — status 200, latency 24 ms, X-Gatehouse-Redaction: mask; address=1; person=2
- **PASS** upstream does not see 王建国 — occurrences upstream: 0
- **PASS** upstream does not see Emily Carter — occurrences upstream: 0
- **PASS** upstream does not see 文澜东路88号 — occurrences upstream: 0
- **PASS** upstream sees placeholders — {{PERSON_qsj65vmo}} {{ADDRESS_g5kezmqy}} {{PERSON_pjg2o3cb}}
- **PASS** code identifiers untouched — the fenced Go code is sent as is
- **PASS** response restored — client receives: "收到：帮我给王建国写封邮件，他住在杭州市西湖区文澜东路88号翠微花园3栋2单元1201室。另外问下 Emily Carter 那个 PR 的进度：\n```go\nfunc (s *AccountService) Disable(ctx context.Context, id int64) error { return n…"

  Upstream received user message: `帮我给{{PERSON_qsj65vmo}}写封邮件，他住在{{ADDRESS_g5kezmqy}}。另外问下 {{PERSON_pjg2o3cb}} 那个 PR 的进度：⏎```go⏎func (s *AccountService) Disable(ctx context.Context, id int64) error { return nil }⏎````

- **PASS** clean request unchanged — status 200, latency 6 ms, X-Gatehouse-Redaction: mask
- **PASS** NER secret masked, not blocked — status 200, X-Gatehouse-Redaction: mask; ner_secret=1
- **PASS** long message (~160 KB) fails closed on the 5 s detect timeout — status 503 after 5017 ms, upstream calls 3 → 3, body {"error":{"message":"detector astrlink: rpc error: code = DeadlineExceeded desc = stream terminated by RST_STREAM with error code: CANCEL; request not sent (fail-closed)","type":"api_error","code":"re…

Plugin process 77332 RSS: 237440 KiB

Gateway log (tail):

```
time=2026-11-25T00:54:05.043-08:00 level=INFO msg="detector plugin started" plugin=astrlink
time=2026-11-25T00:54:05.043-08:00 level=INFO msg="issued the local gateway token" file=<tmp>/local.token
time=2026-11-25T00:54:05.043-08:00 level=INFO msg="gatehouse listening" addr=http://127.0.0.1:50355/v1 mode=personal token_file=<tmp>/local.token handoff=false
time=2026-11-25T00:54:05.080-08:00 level=INFO msg=request id=req_b9c37212b81744ce status=200 model=cloud/strong rule="" dur=23ms ttft_ms=23 body_read_ms=0 req_bytes=380
time=2026-11-25T00:54:05.087-08:00 level=INFO msg=request id=req_b8524f66adff847c status=200 model=cloud/strong rule="" dur=5ms ttft_ms=5 body_read_ms=0 req_bytes=199
time=2026-11-25T00:54:05.093-08:00 level=INFO msg=request id=req_bb8d9a88f746aadb status=200 model=cloud/strong rule="" dur=6ms ttft_ms=6 body_read_ms=0 req_bytes=153
time=2026-11-25T00:54:10.111-08:00 level=INFO msg=request id=req_fb655e8cb35475d8 status=503 model=cloud/strong rule="" dur=5s ttft_ms=0 body_read_ms=0 req_bytes=168555
```

Result: 0 failed check(s).
