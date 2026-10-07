#!/usr/bin/env python3
"""Delay a real Kind gateway handoff beyond idleTimeout; send exactly one query.

Run after verify-wake-on-zero.sh with its Engine parked again. This deliberately
pauses only the wake-agent (which has no liveness probe), after durable wake
acceptance and before releasing the held query. Envoy and the Engine stay live.
The caller must select a local Kind kubeconfig. --expect-stop is a negative
control for a binary with the protection predicate disabled (`protected := false`).
"""
import argparse
import concurrent.futures
import json
import subprocess
import signal
import time
import urllib.error
import urllib.request

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("namespace")
parser.add_argument("--engine", default="engine")
parser.add_argument("--expect-stop", action="store_true")
parser.add_argument("--delay", type=int, default=40)
args = parser.parse_args()


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


initial = engine()
assert initial["spec"]["replicas"] == 0, "Engine must start parked"
assert initial["spec"]["autoStop"]["idleTimeout"] == "25s", "test requires 25s idle timeout"
pods = json.loads(kube("get", "pods", "-l", "firebolt.io/component=gateway", "-o", "json"))
pod = next(p for p in pods["items"] if all(c["ready"] for c in p["status"]["containerStatuses"]))
node = pod["spec"]["nodeName"]
assert run("docker", "inspect", "--format", '{{index .Config.Labels "io.x-k8s.kind.role"}}', node) == "control-plane", "fault injection requires a local Kind node"
container = next(c for c in pod["status"]["containerStatuses"] if c["name"] == "wake-agent")
container_id = container["containerID"].split("://")[1]
info = json.loads(run("docker", "exec", node, "crictl", "inspect", container_id))
pid = str(info["info"]["pid"])
forward = subprocess.Popen(["kubectl", "-n", args.namespace, "port-forward", "pod/" + pod["metadata"]["name"],
                            ":http", ":9903"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
ports = []
while len(ports) < 2:
    line = forward.stdout.readline()
    if not line:
        raise RuntimeError("port-forward exited")
    if line.startswith("Forwarding from 127.0.0.1:"):
        ports.append(int(line.split(":")[1].split()[0]))
http_port, demand_port = ports


def demand():
    with urllib.request.urlopen(f"http://127.0.0.1:{demand_port}/demand", timeout=2) as response:
        return response.read().decode()


def query():
    request = urllib.request.Request(f"http://127.0.0.1:{http_port}/?output_format=JSON_Compact", data=b"SELECT 42",
                                    headers={"X-Firebolt-Engine": args.engine, "Content-Type": "text/plain"})
    try:
        with urllib.request.urlopen(request, timeout=140) as response:
            return response.status, response.read().decode()
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode()


def terminate(_signum, _frame):
    raise SystemExit("interrupted; resuming wake-agent")


signal.signal(signal.SIGTERM, terminate)
paused = False
pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
try:
    started = time.monotonic()
    result = pool.submit(query)
    pending = f'firebolt_gateway_wake_pending_holds{{engine="{args.engine}"}} 1'
    wait_for(lambda: pending in demand())
    previous = initial.get("status", {}).get("lastWakeDemandTime")
    def accepted_wake():
        current = engine()
        if current["spec"]["replicas"] == 1 and current["status"].get("lastWakeDemandTime") != previous:
            return current
        return None

    accepted = wait_for(accepted_wake)
    assert pending in demand(), "query was released before fault injection"
    paused = True
    run("docker", "exec", node, "kill", "-STOP", pid)
    print("Paused wake-agent after acceptance:", accepted["status"]["lastWakeDemandTime"], flush=True)
    stopped = False
    deadline = time.monotonic() + args.delay
    while time.monotonic() < deadline:
        current = engine()
        stopped |= current["spec"]["replicas"] == 0
        assert not result.done(), "query completed before delayed handoff"
        time.sleep(1)
    print("After delayed handoff: replicas=", current["spec"]["replicas"], "phase=", current["status"].get("phase"), flush=True)
    assert stopped == args.expect_stop, f"unexpected idle shutdown: {stopped}"
    run("docker", "exec", node, "kill", "-CONT", pid)
    paused = False
    status, body = result.result(timeout=140)
    print(f"Single query: HTTP {status}, elapsed={time.monotonic()-started:.1f}s; stopped={stopped}", flush=True)
    if args.expect_stop:
        assert status == 503, (status, body)
    else:
        assert status == 200 and "42" in body, (status, body)
finally:
    if paused:
        run("docker", "exec", node, "kill", "-CONT", pid)
    forward.terminate()
    pool.shutdown(wait=True)
