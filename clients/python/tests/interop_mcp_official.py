"""Interoperability check: the official MCP *Python* SDK (an independent
implementation from the Go SDK the bridge is built on) driving the real
`taskworker mcp` executable over stdio.

Requires the optional `mcp` package (install it into a throwaway virtual
environment; it is a development-time check, never a runtime dependency):

    python -m venv .venv && .venv/Scripts/python -m pip install mcp
    TASKWORKER_EXE=... TASKWORKER_FAKEWORKER=... .venv/Scripts/python clients/python/tests/interop_mcp_official.py [modern|legacy]

Prints a JSON evidence object on success; exits non-zero on any failure.
"""

import asyncio
import json
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
sys.path.insert(0, os.path.dirname(__file__))

from scratch import scratch_dir  # noqa: E402

import mcp  # noqa: E402
from mcp import ClientSession, StdioServerParameters  # noqa: E402
from mcp.client.stdio import stdio_client  # noqa: E402

from taskworker_client import Client  # noqa: E402  (independent observer of the same service)

EXE = os.environ["TASKWORKER_EXE"]
FAKE = os.environ["TASKWORKER_FAKEWORKER"]
MODE = sys.argv[1] if len(sys.argv) > 1 else "modern"


def check(cond, message):
    if not cond:
        raise SystemExit("FAIL: " + message)


def body(result):
    text = "".join(c.text for c in result.content if getattr(c, "type", "") == "text")
    return json.loads(text, parse_int=int)


async def run(url):
    import importlib.metadata
    evidence = {"mode": MODE, "mcp_python_sdk": importlib.metadata.version("mcp")}
    params = StdioServerParameters(command=EXE, args=["mcp", "--server", url], env=dict(os.environ))
    async with stdio_client(params) as (read, write):
        async with ClientSession(read, write) as session:
            if MODE == "legacy":
                init = await session.initialize()
                evidence["negotiated_version"] = init.protocol_version
                evidence["server_info"] = init.server_info.name
                caps = init.capabilities
            else:
                disc = await session.discover()
                evidence["negotiated_version"] = str(session.protocol_version)
                evidence["supported_versions"] = list(getattr(disc, "supported_versions", []) or [])
                caps = session.server_capabilities
            check(caps.tools is not None, "tools capability missing")
            check(not (caps.prompts or caps.resources or caps.logging), "unimplemented capabilities advertised")
            evidence["capabilities"] = json.loads(caps.model_dump_json(exclude_none=True))
            if MODE == "legacy":
                await session.send_ping()
                evidence["ping"] = "ok"

            tools = (await session.list_tools()).tools
            names = sorted(t.name for t in tools)
            check(len(names) == 14 and "submit_job" in names and "cancel_job" in names, f"tool list {names}")
            check(all(t.input_schema.get("additionalProperties") is False for t in tools), "schemas must be strict")
            evidence["tools"] = names

            health = body(await session.call_tool("service_health", {}))
            check(health["status"] == "serving" and health["backend"] == "not_checked", health)
            models = body(await session.call_tool("list_models", {}))
            check(models["models"][0]["id"] == "fake:latest", models)

            seed = 9007199254740993  # beyond 2^53: must not be rounded
            args = {"idempotency_key": "interop-" + MODE, "origin": "official-python-sdk",
                    "request": {"model": "fake:latest", "prompt": "héllo 世界", "options": {"max_output_tokens": 8, "seed": seed}}}
            first = await session.call_tool("submit_job", args)
            check(not first.is_error, first)
            job = body(first)["job"]
            check(job["request"]["options"]["seed"] == seed, "seed rounded: %r" % job["request"]["options"]["seed"])
            again = body(await session.call_tool("submit_job", args))
            check(again["job"]["id"] == job["id"], "resend must resolve to the same job")
            waited = body(await session.call_tool("wait_job", {"job_id": job["id"], "timeout_seconds": 20}))
            check(waited["terminal"] and waited["job"]["result"]["text"] == "reply: héllo 世界", waited)
            evidence["submitted_job"] = job["id"]

            hold = body(await session.call_tool("submit_job", {"idempotency_key": "interop-hold-" + MODE, "request": {"model": "fake:latest", "prompt": "hold", "options": {"max_output_tokens": 8}}}))["job"]["id"]
            for _ in range(200):
                page = body(await session.call_tool("read_job_output", {"job_id": hold}))
                if page["text"] == "started":
                    break
                await asyncio.sleep(0.05)
            check(page["text"] == "started", "hold job never produced output")
            timed = body(await session.call_tool("wait_job", {"job_id": hold, "timeout_seconds": 1}))
            check(timed["timed_out"] and not timed["terminal"], timed)

            snap = body(await session.call_tool("list_jobs", {}))
            cursor = snap["cursor"]
            after = f"{cursor['store_id']}:{cursor['sequence']}"
            cancelled = body(await session.call_tool("cancel_job", {"job_id": hold}))
            final = body(await session.call_tool("wait_job", {"job_id": hold, "timeout_seconds": 20}))
            check(final["job"]["state"] == "cancelled" and final["job"]["result"]["text"] == "started", final)
            ev = body(await session.call_tool("read_events", {"after": after, "job_id": hold, "timeout_seconds": 1}))
            states = [e["job"]["state"] for e in ev["events"] if e["kind"] in ("job.state", "job.result")]
            check(states == ["cancelling", "cancelled"], states)
            evidence["cancelled_job"] = hold

            missing = await session.call_tool("get_job", {"job_id": "0" * 32})
            check(missing.is_error and body(missing)["error"]["code"] == "not_found", "fault mapping")
            rejected = await session.call_tool("get_job", {"job_id": "nope", "extra": 1})
            check(rejected.is_error, "schema violation must be a tool error")
            try:
                await session.call_tool("no_such_tool", {})
                check(False, "unknown tool must raise a protocol error")
            except Exception as exc:  # noqa: BLE001 - any protocol-level error is fine
                evidence["unknown_tool"] = type(exc).__name__
    return evidence


def main():
    with scratch_dir("tw-interop-") as d:
        gate = os.path.join(d, "gate")
        os.makedirs(gate)
        svc = subprocess.Popen([FAKE, "--data-dir", os.path.join(d, "data"), "--gate-dir", gate], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        try:
            url = svc.stdout.readline().split()[1]
            evidence = asyncio.run(asyncio.wait_for(run(url), 120))
            # Independent observer: the same jobs through the Python client.
            jobs = {j.id: j for j in Client(url).list_jobs()}
            check(jobs[evidence["submitted_job"]].state == "succeeded", "job not visible via HTTP")
            check(jobs[evidence["cancelled_job"]].state == "cancelled" and jobs[evidence["cancelled_job"]].text == "started", "cancel not visible via HTTP")
            check(len(jobs) == 2, "exactly the two submitted jobs expected: %d" % len(jobs))
            proc = subprocess.run([EXE, "get", "--server", url, evidence["cancelled_job"]], capture_output=True)
            check(proc.returncode == 5, "CLI exit code 5 means an observed cancelled job")
            check(json.loads(proc.stdout.decode("utf-8"))["state"] == "cancelled", "not visible via CLI")
            evidence["visible_via"] = ["python-client", "cli"]
            print(json.dumps(evidence, ensure_ascii=False, indent=2))
        finally:
            svc.stdin.close()
            svc.wait(30)


if __name__ == "__main__":
    main()
