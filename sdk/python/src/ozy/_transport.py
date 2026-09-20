"""UDP transport to the agent: lazy socket, cached DNS, errors surfaced to the caller.

Why UDP: sending a datagram is a single non-blocking syscall with no
connection state, no handshake and no back-pressure. If the agent is down,
slow or missing, the application does not notice — the kernel drops the
packet. That is the right failure mode for metrics, which are statistical
and must never slow down or break the code they measure. The cost is that
delivery is best-effort; ``statsd.stats()`` makes the losses we *can* see
visible.

Design points:

* **Lazy.** Nothing is created until the first send, so importing and
  initializing the SDK is free and a disabled SDK never opens a socket.
* **Connected UDP socket.** ``connect()`` on a UDP socket does no network I/O;
  it only fixes the peer address. The benefit is that ICMP "port unreachable"
  replies are reported back as ``ConnectionRefusedError`` on a later send, so
  a closed agent port shows up in the error counter instead of vanishing.
* **Non-blocking.** A full socket buffer raises ``BlockingIOError`` instead
  of stalling the caller; that datagram is dropped and counted.
* **DNS at most once per 60 s.** ``getaddrinfo`` is blocking and can take
  seconds when DNS is unhealthy, so its result — success *or* failure — is
  cached. Re-resolving periodically still follows an agent whose address
  changes (e.g. a container restart behind a compose service name).

This class is not thread-safe on its own; the client serializes sends.
"""

from __future__ import annotations

import socket
from collections.abc import Callable
from typing import Any

DNS_TTL_SECONDS = 60.0
"""How long a resolution (or resolution failure) is reused before retrying."""

Resolver = Callable[..., list[tuple[Any, ...]]]
"""Shape of ``socket.getaddrinfo``; injectable so tests can fail DNS on demand."""

SocketFactory = Callable[[int, int], socket.socket]
"""Shape of ``socket.socket(family, type)``; injectable so tests can spy on it."""


class UdpTransport:
    """Sends whole datagrams to ``host:port``; raises ``OSError`` on any failure.

    Raising (rather than swallowing) keeps this class honest and small: the
    client decides what a failure means (count it, drop the payload).
    """

    def __init__(
        self,
        host: str,
        port: int,
        *,
        clock: Callable[[], float],
        resolver: Resolver | None = None,
        socket_factory: SocketFactory | None = None,
    ) -> None:
        """Create an idle transport. No socket or DNS lookup happens here.

        Args:
            host: Agent hostname or IP address.
            port: Agent UDP port.
            clock: Monotonic seconds, used for the DNS cache TTL.
            resolver: ``getaddrinfo`` replacement; ``None`` uses the real one,
                looked up at call time so a monkeypatch is honoured.
            socket_factory: ``socket.socket`` replacement; ``None`` likewise.
        """
        self._host = host
        self._port = port
        self._clock = clock
        self._resolver = resolver
        self._socket_factory = socket_factory
        self._sock: socket.socket | None = None
        self._sock_addr: tuple[int, Any] | None = None
        self._resolved_at: float | None = None
        self._addr: tuple[int, Any] | None = None
        self._resolve_error: OSError | None = None

    def _resolve(self) -> tuple[int, Any]:
        now = self._clock()
        if self._resolved_at is None or now - self._resolved_at >= DNS_TTL_SECONDS:
            self._resolved_at = now
            resolver = self._resolver or socket.getaddrinfo
            try:
                infos = resolver(self._host, self._port, 0, socket.SOCK_DGRAM)
                if not infos:
                    raise OSError(f"no addresses for {self._host!r}")
                family, _type, _proto, _canon, sockaddr = infos[0]
                self._addr, self._resolve_error = (family, sockaddr), None
            except (OSError, UnicodeError) as exc:
                # socket.gaierror is an OSError, but getaddrinfo raises
                # UnicodeError for a malformed DNS label (empty, or over 63
                # characters). Letting that escape would leave _addr unset
                # while _resolved_at had advanced, so every send for the next
                # 60 s would fail the assert below instead of raising the
                # OSError this class promises.
                self._addr = None
                self._resolve_error = exc if isinstance(exc, OSError) else OSError(str(exc))
        if self._resolve_error is not None:
            # A fresh exception each time: re-raising the cached object would
            # grow its traceback on every send for the next 60 s.
            raise OSError(f"resolving {self._host!r}: {self._resolve_error}")
        assert self._addr is not None
        return self._addr

    def _socket(self, addr: tuple[int, Any]) -> socket.socket:
        if self._sock is not None and self._sock_addr == addr:
            return self._sock
        self.close()  # the address (or family) changed: start over
        factory = self._socket_factory or socket.socket
        sock = factory(addr[0], socket.SOCK_DGRAM)
        try:
            sock.setblocking(False)
            sock.connect(addr[1])
        except OSError:
            sock.close()
            raise
        self._sock, self._sock_addr = sock, addr
        return sock

    def send(self, data: bytes) -> None:
        """Send one datagram.

        Raises:
            OSError: DNS failed (cached for 60 s), the socket could not be
                created, the kernel buffer is full, or an earlier datagram
                bounced (``ConnectionRefusedError``).
        """
        sock = self._socket(self._resolve())
        try:
            sock.send(data)
        except (BlockingIOError, ConnectionRefusedError):
            raise  # transient: keep the socket
        except OSError:
            self.close()  # unknown state: rebuild on the next send
            raise

    def close(self) -> None:
        """Close the socket if open. Safe to call repeatedly; the next send reopens."""
        sock, self._sock, self._sock_addr = self._sock, None, None
        if sock is not None:
            sock.close()
