"""Buffering: newline coalescing, payload-limit splits, timer flush, flush()/close()."""

from __future__ import annotations

import threading

import pytest

from ozy import Stats, _statsd

from .conftest import ClientFactory, FakeAgent, eventually


def test_messages_coalesce_with_newlines(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client()
    client.increment("a")
    client.gauge("b", 2)
    client.timing("c", 3)
    client.flush()
    assert agent.recv() == "a:1|c\nb:2|g\nc:3|ms"
    assert client.stats() == Stats(sent=3, packets=1, dropped=0, errors=0)


def test_payload_limit_splits_without_exceeding_it(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    # Each line "m.<i>:1|c" is 8 bytes for i < 10; limit 30 fits three
    # (8+1+8+1+8 = 26) but not four (35).
    client = make_client(max_payload=30)
    for i in range(7):
        client.increment(f"m.{i}")
    client.flush()
    datagrams = [agent.recv() for _ in range(3)]
    assert datagrams == [
        "m.0:1|c\nm.1:1|c\nm.2:1|c",
        "m.3:1|c\nm.4:1|c\nm.5:1|c",
        "m.6:1|c",
    ]
    assert all(len(d.encode()) <= 30 for d in datagrams)
    assert client.stats() == Stats(sent=7, packets=3, dropped=0, errors=0)


def test_full_buffer_wakes_flusher_without_explicit_flush(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    # The timer is 60 s here, so only the "buffer full" wake-up can send.
    client = make_client(max_payload=20)
    client.increment("first.metric")
    client.increment("second.metric")  # does not fit: first is handed off
    assert agent.recv() == "first.metric:1|c"
    # The fresh buffer is not sent early by the wake-up.
    assert agent.recv_or_none(0.2) is None
    client.flush()
    assert agent.recv() == "second.metric:1|c"


def test_oversized_message_goes_out_alone(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client(max_payload=10)
    client.increment("short")
    client.increment("a.very.long.metric.name")
    client.flush()
    assert agent.recv() == "short:1|c"
    assert agent.recv() == "a.very.long.metric.name:1|c"


def test_utf8_size_counts_bytes_not_characters(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    # "é" is 2 bytes: two "é:1|c" lines are 6+1+6 = 13 bytes, over a limit of 12.
    client = make_client(max_payload=12)
    client.increment("é")
    client.increment("é")
    client.flush()
    assert agent.recv() == "é:1|c"
    assert agent.recv() == "é:1|c"


def test_timer_flushes_partial_buffer(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client(flush_interval=0.05)
    client.increment("timer.metric")
    assert agent.recv(timeout=2.0) == "timer.metric:1|c"


def test_flusher_thread_starts_lazily_and_once(make_client: ClientFactory) -> None:
    def flushers() -> list[threading.Thread]:
        return [t for t in threading.enumerate() if t.name == "ozy-statsd-flusher"]

    before = len(flushers())
    client = make_client()
    assert len(flushers()) == before  # init alone starts nothing
    client.increment("a")
    client.increment("b")
    assert len(flushers()) == before + 1
    assert all(t.daemon for t in flushers())
    client.close()
    assert eventually(lambda: len(flushers()) == before)


def test_close_flushes_and_disables(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client()
    client.increment("before.close")
    client.close()
    assert agent.recv() == "before.close:1|c"
    assert not client.enabled
    client.increment("after.close")
    client.flush()
    client.close()  # idempotent
    assert agent.recv_or_none(0.1) is None


def test_sampled_out_calls_are_not_counted(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client(random=lambda: 0.9)
    client.increment("sampled.out", sample_rate=0.5)
    client.increment("sampled.in", sample_rate=0.95)
    client.flush()
    assert agent.recv() == "sampled.in:1|c|@0.95"
    assert client.stats() == Stats(sent=1, packets=1, dropped=0, errors=0)


def test_flush_with_empty_buffer_sends_nothing(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    client.flush()
    assert agent.recv_or_none(0.1) is None


def test_pending_queue_is_bounded_and_drops_oldest(
    make_client: ClientFactory, agent: FakeAgent, monkeypatch: pytest.MonkeyPatch
) -> None:
    client = make_client(max_payload=8)
    # No flusher thread: this is "the sender is stuck and nothing drains".
    monkeypatch.setattr(client, "_ensure_thread_locked", lambda: None)
    total = _statsd.MAX_PENDING_PAYLOADS + 5
    for i in range(total):
        client.increment(f"q{i:03d}")  # 9 bytes each: one payload per line
    # total-1 payloads were queued (the last is still buffered); 4 evicted.
    assert client.stats().dropped == total - 1 - _statsd.MAX_PENDING_PAYLOADS
    assert len(client._pending) == _statsd.MAX_PENDING_PAYLOADS
    client.flush()
    received = []
    while (d := agent.recv_or_none(0.2)) is not None:
        received.append(d)
    # The newest message survived; the oldest did not.
    assert received[0] == "q004:1|c"
    assert received[-1] == f"q{total - 1:03d}:1|c"
    stats = client.stats()
    assert (stats.sent, stats.packets, stats.dropped) == (total - 4, total - 4, 4)


def test_many_threads_lose_nothing(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client(max_payload=8192)
    n_threads, per_thread = 8, 200

    def work() -> None:
        for _ in range(per_thread):
            client.increment("x")

    threads = [threading.Thread(target=work) for _ in range(n_threads)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    client.flush()
    lines = 0
    while (d := agent.recv_or_none(0.2)) is not None:
        lines += len(d.split("\n"))
    assert lines == n_threads * per_thread
    assert client.stats().sent == n_threads * per_thread
