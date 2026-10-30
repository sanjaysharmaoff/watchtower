"""Push realistic application logs into Loki.

This is the stand-in for an application in the e2e stack: it writes the kind of
lines a failing service actually produces, with duplicates, high cardinality
request ids and an accidentally logged credential, so WatchTower's sanitizer and
input guardrail have real work to do.
"""

import json
import os
import random
import time
import urllib.error
import urllib.request

LOKI = os.environ.get("LOKI_URL", "http://loki:3100")
PUSH = LOKI.rstrip("/") + "/loki/api/v1/push"
INTERVAL = float(os.environ.get("INTERVAL_SECONDS", "5"))

STREAMS = {
    ("checkout", "checkout-api", "checkout-api-7d9c6f8b4-x2ktz"): [
        ("error", "fatal error: runtime: out of memory (heap 512MiB, limit 512MiB) req_id=%08x"),
        ("error", "container terminated: OOMKilled after 12m uptime req_id=%08x"),
        ("warn", "gc pressure high: 94%% of heap in use, pause 412ms req_id=%08x"),
        ("warn", "retrying upstream call to payments-api (attempt 3 of 5) req_id=%08x"),
        ("error", "context deadline exceeded calling payments-api:8080 req_id=%08x"),
        ("info", "starting checkout-api version 4.2.1 dsn=postgres://svc:s3cr3tp4ss@db.internal:5432/checkout req_id=%08x"),
        ("info", "handled GET /api/v1/cart in 41ms req_id=%08x"),
        ("debug", "cache lookup hit ratio 0.82 req_id=%08x"),
    ],
    ("checkout", "payments-api", "payments-api-5f8b7c6d5-mm41k"): [
        ("error", "connection refused dialing payments-db:5432 req_id=%08x"),
        ("warn", "circuit breaker half-open for payments-db req_id=%08x"),
        ("info", "healthz ok req_id=%08x"),
    ],
}


def batch():
    streams = []
    for (namespace, app, pod), templates in STREAMS.items():
        values = []
        now = time.time_ns()
        for i in range(random.randint(6, 14)):
            level, template = random.choice(templates)
            line = template % random.getrandbits(32)
            ts = now - i * 250_000_000
            values.append([str(ts), f"level={level} msg=\"{line}\""])
        values.reverse()
        streams.append(
            {
                "stream": {
                    "namespace": namespace,
                    "app": app,
                    "pod": pod,
                    "cluster": "prod-eu-1",
                },
                "values": values,
            }
        )
    return {"streams": streams}


def main():
    print(f"pushing logs to {PUSH} every {INTERVAL}s", flush=True)
    while True:
        payload = json.dumps(batch()).encode()
        req = urllib.request.Request(
            PUSH, data=payload, headers={"Content-Type": "application/json"}, method="POST"
        )
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                if resp.status >= 300:
                    print(f"loki push status {resp.status}", flush=True)
        except urllib.error.HTTPError as err:
            print(f"loki push failed: {err.code} {err.read()[:200]}", flush=True)
        except Exception as err:  # noqa: BLE001 - keep the generator alive
            print(f"loki push error: {err}", flush=True)
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
