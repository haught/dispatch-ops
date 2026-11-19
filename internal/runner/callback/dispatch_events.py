# Dispatch Ops callback plugin. The runner writes this file into the job's
# workspace, puts its directory on ANSIBLE_CALLBACK_PLUGINS and makes it the
# stdout callback; it is not meant to be installed by hand.
from __future__ import annotations

DOCUMENTATION = r"""
name: dispatch_events
type: stdout
short_description: Ansible's default output, with structured events for Dispatch Ops
description:
  - Prints what the default callback prints. Around the output of each play,
    task and host result, it writes a line opening a structured event and a
    line closing it, prefixed with the marker in DISPATCH_EVENT_MARKER. The
    runner stores each as one event, the structured data with the text
    printed in between as its output.
  - Without DISPATCH_EVENT_MARKER, it is the default callback.
extends_documentation_fragment:
  - default_callback
  - result_format_callback
requirements:
  - set as stdout callback
"""

import json
import os
import sys

from ansible.plugins.callback.default import CallbackModule as DefaultCallback

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


def _field(result, name):
    # ansible-core 2.19 made the task result's fields public.
    value = getattr(result, name, None)
    return value if value is not None else getattr(result, "_" + name)


class CallbackModule(DefaultCallback):
    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = "stdout"
    CALLBACK_NAME = "dispatch_events"

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._marker = os.environ.get("DISPATCH_EVENT_MARKER", "")

    def _write(self, line):
        # The same stream as the default output, so the runner sees both in order.
        sys.stdout.write(self._marker + line + "\n")
        sys.stdout.flush()

    def _open(self, event_type, data, host=None, task=None):
        if not self._marker:
            return
        event = {"type": event_type, "data": data}
        if host is not None:
            event["host"] = host
        if task is not None:
            event["task"] = task._uuid
        line = json.dumps(event, default=str, ensure_ascii=False, separators=(",", ":"))
        if len(line.encode("utf-8")) > MAX_LINE:
            keep = ("task", "action", "play", "changed", "ignored", "rc", "handler")
            event["data"] = {k: v for k, v in data.items() if k in keep}
            event["data"]["truncated"] = True
            line = json.dumps(event, default=str, ensure_ascii=False, separators=(",", ":"))
        self._write(line)

    def _close(self):
        if self._marker:
            self._write(".")

    def _wrap(self, event_type, data, show, host=None, task=None):
        """Opens an event, prints the default output with show(), and closes it."""
        self._open(event_type, data, host=host, task=task)
        try:
            show()
        finally:
            self._close()

    def _play_name(self):
        return self._play.get_name().strip() if self._play else ""

    def _task_start_data(self, task, handler=False):
        data = {"task": task.get_name().strip(), "action": task.action, "play": self._play_name()}
        if handler:
            data["handler"] = True
        return data

    def _result(self, event_type, result, show, **extra):
        res = _field(result, "result")
        data = {"changed": bool(res.get("changed", False))}
        data.update(extra)
        if res.get("_ansible_no_log"):
            data["msg"] = HIDDEN
        else:
            for key in ("msg", "stdout", "stderr"):
                if res.get(key):
                    data[key] = _text(res[key])
            if isinstance(res.get("rc"), int):
                data["rc"] = res["rc"]
        host = _field(result, "host").get_name()
        self._wrap(event_type, data, show, host=host, task=_field(result, "task"))

    def v2_playbook_on_start(self, playbook):
        self._wrap("playbook_start", {"playbook": os.path.basename(playbook._file_name)},
                   lambda: super(CallbackModule, self).v2_playbook_on_start(playbook))

    def v2_playbook_on_play_start(self, play):
        self._wrap("play_start", {"play": play.get_name().strip()},
                   lambda: super(CallbackModule, self).v2_playbook_on_play_start(play))

    def v2_playbook_on_task_start(self, task, is_conditional):
        # The default callback sets the play, which play_start does first.
        self._wrap("task_start", self._task_start_data(task),
                   lambda: super(CallbackModule, self).v2_playbook_on_task_start(task, is_conditional), task=task)

    def v2_playbook_on_handler_task_start(self, task):
        self._wrap("task_start", self._task_start_data(task, handler=True),
                   lambda: super(CallbackModule, self).v2_playbook_on_handler_task_start(task), task=task)

    def v2_runner_on_ok(self, result):
        self._result("host_ok", result, lambda: super(CallbackModule, self).v2_runner_on_ok(result))

    def v2_runner_on_failed(self, result, ignore_errors=False):
        self._result("host_failed", result,
                     lambda: super(CallbackModule, self).v2_runner_on_failed(result, ignore_errors=ignore_errors),
                     ignored=bool(ignore_errors))

    def v2_runner_on_skipped(self, result):
        self._result("host_skipped", result, lambda: super(CallbackModule, self).v2_runner_on_skipped(result))

    def v2_runner_on_unreachable(self, result):
        self._result("host_unreachable", result, lambda: super(CallbackModule, self).v2_runner_on_unreachable(result))

    def v2_playbook_on_stats(self, stats):
        hosts = sorted(stats.processed.keys())
        for i in range(0, max(len(hosts), 1), STATS_CHUNK):
            chunk = {h: stats.summarize(h) for h in hosts[i : i + STATS_CHUNK]}
            self._wrap("playbook_stats", {"hosts": chunk}, lambda: None)
        # The recap as Ansible prints it, as plain output.
        super().v2_playbook_on_stats(stats)
