#!/usr/bin/env python3
"""Guard the delayed-handoff test against false-positive observations."""
import importlib.util
from pathlib import Path
import unittest
from unittest import mock
import subprocess
import sys
import time
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location("handoff", Path(__file__).with_name("verify-wake-handoff.py"))
handoff = importlib.util.module_from_spec(spec)
spec.loader.exec_module(handoff)


class HandoffObservationTests(unittest.TestCase):
    def test_rollout_readiness_is_not_stability(self):
        for phase, ready, expected in [("creating", 1, False), ("stable", 0, False), ("stable", 1, True)]:
            self.assertEqual(handoff.stable_ready({"status": {"phase": phase, "readyReplicas": ready}}), expected)

    def test_generation_catches_churn_between_reads(self):
        initial = {"metadata": {"uid": "engine", "generation": 10}}
        current = {"metadata": {"uid": "engine", "generation": 11}, "spec": {"replicas": 1}}
        self.assertFalse(handoff.capacity_changed(current, initial, 11))
        # Replicas are one in both samples; the unseen down/up writes remain visible.
        current["metadata"]["generation"] = 13
        self.assertTrue(handoff.capacity_changed(current, initial, 11))
        current["metadata"] = {"uid": "replacement", "generation": 11}
        self.assertTrue(handoff.capacity_changed(current, initial, 11))


class PortForwardLifecycleTests(unittest.TestCase):
    def exercise(self, child_code, check):
        children = []
        original = subprocess.Popen
        def spawn(*args, **kwargs):
            child = original(*args, **kwargs)
            children.append(child)
            return child
        try:
            with mock.patch.object(handoff.subprocess, "Popen", side_effect=spawn):
                check([sys.executable, "-u", "-c", child_code])
            self.assertEqual(len(children), 1)
            self.assertIsNotNone(children[0].returncode, "child was not reaped")
        finally:
            for child in children:
                if child.poll() is None:
                    child.kill()
                    child.wait(timeout=2)

    def test_silent_startup_times_out_and_reaps_child(self):
        def check(command):
            started = time.monotonic()
            with self.assertRaises(TimeoutError):
                with handoff.port_forward(command, [8080, 9903], startup_timeout=0.15):
                    self.fail("silent child was considered ready")
            self.assertLess(time.monotonic() - started, 2)
        self.exercise("import time; time.sleep(60)", check)

    def test_interrupted_startup_reaps_child(self):
        def check(command):
            with mock.patch.object(handoff, "time", SimpleNamespace(
                    monotonic=time.monotonic, sleep=mock.Mock(side_effect=KeyboardInterrupt))):
                with self.assertRaises(KeyboardInterrupt):
                    with handoff.port_forward(command, [8080, 9903]):
                        self.fail("silent child was considered ready")
        self.exercise("import time; time.sleep(60)", check)

    def test_early_failure_reaps_child(self):
        def check(command):
            with self.assertRaisesRegex(RuntimeError, "port-forward exited"):
                with handoff.port_forward(command, [8080, 9903]):
                    self.fail("failed child was considered ready")
        self.exercise("raise SystemExit(2)", check)

    def test_interrupted_body_reaps_child_and_ports_follow_targets(self):
        def check(command):
            with self.assertRaises(KeyboardInterrupt):
                with handoff.port_forward(command, [8080, 9903]) as ports:
                    self.assertEqual(ports, [1234, 5678])
                    raise KeyboardInterrupt()
        self.exercise("import time; print('Forwarding from 127.0.0.1:5678 -> 9903'); "
                      "print('Forwarding from 127.0.0.1:1234 -> 8080'); time.sleep(60)", check)


if __name__ == "__main__":
    unittest.main()
