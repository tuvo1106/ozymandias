"""A Flask app traced by ozymandias: one ``init`` call, no instrumentation in the handlers.

The app owners' services are FastAPI (ASGI) and Next.js; this one exists to prove the Python
SDK on the other half of the Python web world, WSGI, through the ``flask`` integration:

- ``ozy.init(integrations=["flask", "logging"])`` wraps every Flask app's ``wsgi_app``. Each
  request becomes an ``http.request`` span whose resource is Flask's route rule
  (``GET /items/<int:item_id>``), plus the ``http.request.count`` / ``.duration`` metrics.
- The one manual span is the database call: the stdlib ``sqlite3`` module has no integration,
  and ``tracer.trace`` is the API an integration would use anyway. It is a child of the
  request span because the request span is active while the handler runs.
- A log line written inside a request carries ``trace_id`` / ``span_id`` (the ``logging``
  integration), which is how a log line leads to its trace.

Run it::

    OZY_AGENT_HOST=localhost uv run app.py           # serve on :5050
    OZY_AGENT_HOST=localhost uv run app.py --drive   # serve on a free port, send a few
                                                     # requests through it, print the trace id

With ``OZY_AGENT_HOST`` unset the SDK is inert and the app works exactly the same.
"""

from __future__ import annotations

import json
import logging
import sqlite3
import sys
import threading
import time
import urllib.error
import urllib.request

from flask import Flask, abort, g

import ozy

ozy.init(service="flask-example", env="dev", integrations=["flask", "logging"])
log = logging.getLogger("flask-example")
_handler = logging.StreamHandler()
# The default keeps the format valid when the SDK is inert and nothing stamps the record.
_handler.setFormatter(
    logging.Formatter(
        "%(asctime)s %(levelname)s %(message)s trace_id=%(trace_id)s", defaults={"trace_id": "-"}
    )
)
logging.basicConfig(level=logging.INFO, handlers=[_handler])

app = Flask(__name__)

SCHEMA = """
create table items (id integer primary key, name text not null);
insert into items (name) values ('anvil'), ('bellows'), ('crucible');
"""


def db() -> sqlite3.Connection:
    """One in-memory database per request: the example has no state worth keeping."""
    if "db" not in g:
        g.db = sqlite3.connect(":memory:")
        g.db.executescript(SCHEMA)
    return g.db


def query(sql: str, *params: object) -> list[tuple[object, ...]]:
    """Run ``sql`` inside a ``sqlite.query`` span.

    The resource is the statement, never the parameters: a bound value can be an email or a
    token, and a span is not the place for it.
    """
    with ozy.tracer.trace("sqlite.query", resource=sql, type="db") as span:
        rows = db().execute(sql, params).fetchall()
        span.set_metric("db.rowcount", len(rows))
        return rows


@app.teardown_appcontext
def close_db(_exc: BaseException | None) -> None:
    conn = g.pop("db", None)
    if conn is not None:
        conn.close()


@app.get("/items")
def list_items() -> list[dict[str, object]]:
    return [{"id": i, "name": n} for i, n in query("select id, name from items order by id")]


@app.get("/items/<int:item_id>")
def get_item(item_id: int) -> dict[str, object]:
    rows = query("select id, name from items where id = ?", item_id)
    if not rows:
        abort(404)
    log.info("served item %s", item_id)
    return {"id": rows[0][0], "name": rows[0][1]}


@app.get("/boom")
def boom() -> str:
    raise RuntimeError("the example failed on purpose")


@app.get("/healthz")
def healthz() -> str:
    return "ok"


def drive() -> None:
    """Serve on a free port with werkzeug's real WSGI server and send requests through it.

    A real server, not Flask's test client, because the middleware records a request when
    the server closes the response body, and that is the part worth exercising. All the
    requests share one trace: the driver's own span is injected into each request's
    headers, the way one service calls another.
    """
    from werkzeug.serving import make_server

    server = make_server("127.0.0.1", 0, app, threaded=True)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{server.server_port}"
    statuses = {}
    with ozy.tracer.trace("example.drive", resource="flask-example drive") as root:
        for path in ["/items", "/items/1", "/items/2", "/items/999", "/nope/42", "/boom"]:
            headers = ozy.tracer.inject({})
            request = urllib.request.Request(base + path, headers=headers)
            try:
                with urllib.request.urlopen(request, timeout=5) as response:
                    response.read()  # a client that hangs up early makes werkzeug skip close()
                    statuses[path] = response.status
            except urllib.error.HTTPError as err:
                err.read()
                statuses[path] = err.code
    server.shutdown()
    # A request span ends when the server closes the response body, on the handler thread,
    # after the client already has its answer. Werkzeug's handler threads are daemons that
    # nothing joins, so wait until every span that started has finished (a no-op when the
    # SDK is inert: both counts are zero).
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        stats = ozy.tracer.stats()
        if stats.spans_finished >= stats.spans_started:
            break
        time.sleep(0.01)
    ozy.tracer.flush()
    ozy.statsd.flush()
    print(json.dumps({"trace_id": root.trace_id, "statuses": statuses}))


if __name__ == "__main__":
    if "--drive" in sys.argv:
        drive()
    else:
        app.run(host="127.0.0.1", port=5050)
