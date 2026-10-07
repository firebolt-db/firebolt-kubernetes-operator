#!/usr/bin/env python3
"""Delay a real gateway handoff beyond idleTimeout; send exactly one query.

Requires the CI-only wakehandofftest build. Arm its barrier before sending the
query, then release after the Engine has been stable for the idle window.
--expect-stop is a negative control for a binary with protection disabled.
"""
import argparse
from contextlib import contextmanager
import re
import os
import tempfile
import concurrent.futures
import json
import subprocess
import signal
import time
import urllib.error
import urllib.request


def run(*argv):
    return subprocess.check_output(argv, text=True, timeout=15).strip()


def kube(*argv):
    return run("kubectl", "-n", args.namespace, *argv)


def engine():
    return json.loads(kube("get", "fireboltengine", args.engine, "-o", "json"))


def wait_for(check, seconds=15):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.1)
    raise RuntimeError("condition did not converge within the test deadline")


def terminate(_signum, _frame):
    raise SystemExit("interrupted; cleaning up handoff test")


def stable_ready(current):
    status = current.get("status", {})
    return status.get("phase") == "stable" and status.get("readyReplicas", 0) == 1


def capacity_changed(current, initial, expected_generation):
    return (current["metadata"]["uid"] != initial["metadata"]["uid"] or
            current["metadata"]["generation"] != expected_generation or
            current["spec"]["replicas"] != 1)


@contextmanager
def port_forward(command, targets, startup_timeout=15):
    # A regular file can be read without blocking on a quiet child. Keep its
    # lifetime inside cleanup, including startup failures and interruptions.
    with tempfile.TemporaryFile() as output:
        process = None
        try:
            process = subprocess.Popen(command, stdout=output, stderr=subprocess.STDOUT, text=True)
            deadline = time.monotonic() + startup_timeout
            while True:
                text = os.pread(output.fileno(), 65536, 0).decode(errors="replace")
                ports = {int(target): int(local) for local, target in
                         re.findall(r"Forwarding from 127\.0\.0\.1:(\d+) -> (\d+)", text)}
                if all(target in ports for target in targets):
                    yield [ports[target] for target in targets]
                    return
                if process.poll() is not None:
                    raise RuntimeError(f"port-forward exited: {text}")
                if time.monotonic() >= deadline:
                    raise TimeoutError(f"port-forward startup timed out: {text}")
                time.sleep(0.05)
        finally:
            if process is not None:
                if process.poll() is None:
                    process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


def main():
    global args
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("namespace")
    parser.add_argument("--engine", default="engine")
    parser.add_argument("--expect-stop", action="store_true")
    parser.add_argument("--delay", type=int, default=40)
    args = parser.parse_args()
    assert 35 <= args.delay <= 60, "delay must exceed 25s idle + 5s poll and fit the 120s gateway deadline"

    initial = wait_for(lambda: (e if (e := engine())["spec"]["replicas"] == 0 and
                            e.get("status", {}).get("phase") == "stopped" else None), seconds=180)
    assert initial["spec"]["replicas"] == 0, "Engine must start parked"
    assert initial["spec"]["autoStop"]["idleTimeout"] == "25s", "test requires 25s idle timeout"
    assert initial["spec"]["autoStop"].get("pollInterval") == "5s", "test requires 5s auto-stop polling"
    pods = json.loads(kube("get", "pods", "-l", "firebolt.io/component=gateway", "-o", "json"))
    pod = next(p for p in pods["items"] if all(c["ready"] for c in p["status"]["containerStatuses"]))
    http_target = next(port["containerPort"] for container in pod["spec"]["containers"]
                       for port in container.get("ports", []) if port["name"] == "http")
    signal.signal(signal.SIGTERM, terminate)
    command = ["kubectl", "-n", args.namespace, "port-forward", "pod/" + pod["metadata"]["name"],
               f":{http_target}", ":9903"]
    with port_forward(command, [http_target, 9903]) as (http_port, demand_port):
        run_handoff(initial, http_port, demand_port)


def run_handoff(initial, http_port, demand_port):
    def barrier(method):
        request = urllib.request.Request(
            f"http://127.0.0.1:{demand_port}/test/handoff?engine={args.engine}", method=method)
        try:
            with urllib.request.urlopen(request, timeout=2) as response:
                return response.status
        except urllib.error.HTTPError as error:
            raise RuntimeError("CI handoff barrier unavailable; build with GO_BUILD_TAGS=latest,wakehandofftest") from error

    def query():
        request = urllib.request.Request(f"http://127.0.0.1:{http_port}/?output_format=JSON_Compact", data=b"SELECT 42",
                                        headers={"X-Firebolt-Engine": args.engine, "Content-Type": "text/plain"})
        try:
            with urllib.request.urlopen(request, timeout=140) as response:
                return response.status, response.read().decode()
        except urllib.error.HTTPError as error:
            return error.code, error.read().decode()

    armed = False
    pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    try:
        # POST returns 202 until this selected agent is synced and observes
        # no ready endpoints. Only a 201 arms the barrier and permits the query.
        armed = True
        wait_for(lambda: barrier("POST") == 201, seconds=60)
        started = time.monotonic()
        result = pool.submit(query)
        wait_for(lambda: barrier("GET") == 200)
        previous = initial.get("status", {}).get("lastWakeDemandTime")
        def accepted_wake():
            current = engine()
            if current["spec"]["replicas"] == 1 and current["status"].get("lastWakeDemandTime") != previous:
                assert current["status"].get("wakeProtectionUntil"), "wake deadline was not persisted"
                return current
            return None

        accepted = wait_for(accepted_wake)
        print("Query blocked at CI barrier after acceptance:", accepted["status"]["lastWakeDemandTime"], flush=True)
        # Exactly one spec mutation is allowed: the query's zero-to-one wake.
        # Generation is durable, so even a complete 1->0->1 between GETs is visible.
        expected_generation = initial["metadata"]["generation"] + 1
        current = accepted
        stopped = capacity_changed(accepted, initial, expected_generation)
        assert args.expect_stop or not stopped, "replicas churned before acceptance was observed"
        stable_deadline = started + 120 - args.delay - 10
        while not stopped:
            current = engine()
            stopped = capacity_changed(current, initial, expected_generation)
            assert args.expect_stop or not stopped, "replicas changed before stable handoff"
            assert not result.done(), "query completed before delayed handoff"
            if stopped or stable_ready(current):
                break
            assert time.monotonic() < stable_deadline, "startup left insufficient time inside the gateway hold deadline"
            time.sleep(0.2)
        if not stopped:
            print("Engine stable and ready; starting idle-eligible delay", flush=True)
            deadline = time.monotonic() + args.delay
            while True:
                current = engine()
                stopped |= capacity_changed(current, initial, expected_generation)
                assert args.expect_stop or not stopped, "replicas changed during delayed handoff"
                assert not result.done(), "query completed before delayed handoff"
                if time.monotonic() >= deadline:
                    break
                time.sleep(0.5)
        print("After delayed handoff: replicas=", current["spec"]["replicas"], "phase=", current["status"].get("phase"), flush=True)
        assert stopped == args.expect_stop, f"unexpected idle shutdown: {stopped}"
        assert barrier("DELETE") == 204
        armed = False
        status, body = result.result(timeout=140)
        print(f"Single query: HTTP {status}, elapsed={time.monotonic()-started:.1f}s; stopped={stopped}", flush=True)
        if args.expect_stop:
            assert status == 503, (status, body)
        else:
            assert status == 200 and "42" in body, (status, body)
            assert not capacity_changed(engine(), initial, expected_generation), "replicas changed before query completion"
    finally:
        try:
            if armed:
                barrier("DELETE")
        finally:
            pool.shutdown(wait=True)


if __name__ == "__main__":
    main()
