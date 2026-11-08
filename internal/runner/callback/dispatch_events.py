# Dispatch Ops callback plugin. The runner writes this file into the job's
# workspace and puts its directory on ANSIBLE_CALLBACK_PLUGINS; it is not
# meant to be installed by hand.
from __future__ import annotations

DOCUMENTATION = r"""
name: dispatch_events
type: notification
short_description: Reports structured job events to the Dispatch Ops runner
description:
  - Writes one line per playbook, play, task, host result and recap to
    standard output, prefixed with the marker in DISPATCH_EVENT_MARKER. The
    runner turns these lines into structured job events instead of output.
  - Does nothing unless DISPATCH_EVENT_MARKER is set.
"""

import json
import os
import sys

from ansible.plugins.callback import CallbackBase

# Longest text kept from a result field (msg, stdout, stderr).
MAX_TEXT = 2048
# Longest event line. The runner splits longer lines, so stay well below 16 KiB.
MAX_LINE = 12 * 1024
# Hosts per playbook_stats event.
STATS_CHUNK = 50

HIDDEN = "the output has been hidden due to the fact that 'no_log: true' was specified for this result"


def _text(value):
    if not isinstance(value, str):
        value = json.dumps(value, default=str, ensure_ascii=False)
    if len(value) > MAX_TEXT:
        value = value[:MAX_TEXT] + " …"
    return value


class CallbackModule(CallbackBase):
    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = "notification"
    CALLBACK_NAME = "dispatch_events"
    # Loaded whenever it is on the callback plugin path.
    CALLBACK_NEEDS_ENABLED = False

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._marker = os.environ.get("DISPATCH_EVENT_MARKER", "")
        self._play = ""

    def _emit(self, event_type, data, host=None):
        if not self._marker:
            return
        event = {"type": event_type, "data": data}
        if host is not None:
            event["host"] = host
        line = json.dumps(event, default=str, ensure_ascii=False, separators=(",", ":"))
        if len(line.encode("utf-8")) > MAX_LINE:
            keep = ("task", "action", "play", "changed", "ignored", "rc")
            event["data"] = {k: v for k, v in data.items() if k in keep}
            event["data"]["truncated"] = True
            line = json.dumps(event, default=str, ensure_ascii=False, separators=(",", ":"))
        # The same stream as Ansible's own output, so the runner sees both in order.
        sys.stdout.write(self._marker + line + "\n")
        sys.stdout.flush()

    def _task_data(self, task):
        return {"task": task.get_name().strip(), "action": task.action, "play": self._play}

    def _result(self, event_type, result, **extra):
        res = result._result
        data = self._task_data(result._task)
        data["changed"] = bool(res.get("changed", False))
        data.update(extra)
        if res.get("_ansible_no_log"):
            data["msg"] = HIDDEN
        else:
            for key in ("msg", "stdout", "stderr"):
                if res.get(key):
                    data[key] = _text(res[key])
            if isinstance(res.get("rc"), int):
                data["rc"] = res["rc"]
        self._emit(event_type, data, host=result._host.get_name())

    def v2_playbook_on_start(self, playbook):
        self._emit("playbook_start", {"playbook": os.path.basename(playbook._file_name)})

    def v2_playbook_on_play_start(self, play):
        self._play = play.get_name().strip()
        self._emit("play_start", {"play": self._play})

    def v2_playbook_on_task_start(self, task, is_conditional):
        self._emit("task_start", self._task_data(task))

    def v2_playbook_on_handler_task_start(self, task):
        data = self._task_data(task)
        data["handler"] = True
        self._emit("task_start", data)

    def v2_runner_on_ok(self, result):
        self._result("host_ok", result)

    def v2_runner_on_failed(self, result, ignore_errors=False):
        self._result("host_failed", result, ignored=bool(ignore_errors))

    def v2_runner_on_skipped(self, result):
        self._result("host_skipped", result)

    def v2_runner_on_unreachable(self, result):
        self._result("host_unreachable", result)

    def v2_playbook_on_stats(self, stats):
        hosts = sorted(stats.processed.keys())
        for i in range(0, max(len(hosts), 1), STATS_CHUNK):
            chunk = {h: stats.summarize(h) for h in hosts[i : i + STATS_CHUNK]}
            self._emit("playbook_stats", {"hosts": chunk})
