"""Fork safety: the child gets a fresh flusher, its own socket and none of the parent's buffer."""

from __future__ import annotations

import os
import threading
import time
import warnings

import pytest

from .conftest import ClientFactory, FakeAgent

pytestmark = pytest.mark.skipif(not hasattr(os, "fork"), reason="platform has no fork()")


def test_child_restarts_flusher_and_sends(make_client: ClientFactory, agent: FakeAgent) -> None:
    # Short timer so the child's *background* flusher (not an explicit
    # flush) must deliver its metric.
    client = make_client(flush_interval=0.05)

    # Parent: get the flusher running, then fork while holding the buffer
    # lock with an unsent line in the buffer: the worst moment to fork.
    client.increment("parent.started")
    assert agent.recv() == "parent.started:1|c"
    parent_thread = client._thread
    assert parent_thread is not None
    assert parent_thread.is_alive()

    read_fd, write_fd = os.pipe()
    with client._lock:
        client._buffer.append("parent.unflushed:1|c")
        client._buffer_size += len("parent.unflushed:1|c")
        with warnings.catch_warnings():
            # 3.12+ warns that forking a multi-threaded process may deadlock;
            # surviving exactly that is what this test is about.
            warnings.simplefilter("ignore", DeprecationWarning)
            pid = os.fork()

    if pid == 0:  # child
        status = 1
        try:
            os.close(read_fd)
            ok = (
                client._thread is None  # the parent's thread did not survive
                and client._buffer == []  # the parent's messages stay the parent's
                and client._lock.acquire(timeout=1)  # not inherited in the locked state
            )
            if ok:
                client._lock.release()
                client.increment("child.metric")
                thread = client._thread
                ok = (
                    thread is not None
                    and thread.is_alive()
                    and thread is not parent_thread
                    and thread.name in {t.name for t in threading.enumerate()}
                )
                deadline = time.monotonic() + 2
                while ok and client.stats().sent < 1 and time.monotonic() < deadline:
                    time.sleep(0.01)
                ok = ok and client.stats().sent == 1
            os.write(write_fd, b"ok" if ok else b"fail")
            status = 0 if ok else 1
        finally:
            os._exit(status)

    # parent
    os.close(write_fd)
    _, wait_status = os.waitpid(pid, 0)
    report = os.read(read_fd, 16)
    os.close(read_fd)
    assert report == b"ok"
    assert os.waitstatus_to_exitcode(wait_status) == 0

    # The child's datagram arrives exactly once, alone.
    received = []
    client.flush()  # parent sends its own buffered line
    while (d := agent.recv_or_none(0.5)) is not None:
        received.extend(d.split("\n"))
    assert received.count("child.metric:1|c") == 1
    assert received.count("parent.unflushed:1|c") == 1
    # The parent's flusher is unaffected.
    assert parent_thread.is_alive()
