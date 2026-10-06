"""One trace across the queue, through a real arq worker and a real Redis.

Needs a Redis. Set ``OZY_TEST_REDIS_URL`` to a *dedicated, non-zero database* (the test
flushes it), for example ``redis://localhost:6379/15``. Unset, the test is skipped: CI has
no Redis container yet (docs/plan/M5-tracing.md L8 calls for one, marked with a docker marker
``docker`` marker), so this is verified by hand until it has one.
"""

from __future__ import annotations

import asyncio
import os
import uuid
from typing import Any
from urllib.parse import urlparse

import arq
import httpx
import pytest
from arq import create_pool
from arq.connections import RedisSettings
from arq.worker import Worker
from fastapi import FastAPI
from sqlalchemy import create_engine, text

import ozy
from ozy.integrations import arq as ozy_arq
from ozy.integrations import sqlalchemy as ozy_sa
from ozy.integrations.asgi import TraceMiddleware

from .conftest import FakeTraceAgent

URL = os.environ.get("OZY_TEST_REDIS_URL", "")
DB = urlparse(URL).path.lstrip("/") if URL else ""

pytestmark = pytest.mark.skipif(
    not URL or DB in ("", "0"),
    reason="set OZY_TEST_REDIS_URL to a dedicated non-zero redis database to run",
)


def test_http_request_to_enqueue_to_job_to_query_is_one_trace(traced: FakeTraceAgent) -> None:
    asyncio.run(scenario(traced))


async def scenario(agent: FakeTraceAgent) -> None:
    queue = f"ozy-test-{uuid.uuid4().hex[:8]}"
    engine = create_engine("sqlite://")
    ozy_arq.INTEGRATION.patch()
    ozy_sa.INTEGRATION.patch()
    received: dict[str, Any] = {}

    async def judge(ctx: dict[str, Any], submission_id: int) -> str:
        # Strict signature: a leaked _ozymandias kwarg would be a TypeError right here.
        received["submission_id"] = submission_id
        with engine.connect() as conn:
            conn.execute(text("SELECT :n"), {"n": submission_id})
        return "accepted"

    pool = await create_pool(RedisSettings.from_dsn(URL), default_queue_name=queue)
    try:
        await pool.flushdb()
        app = FastAPI()

        @app.post("/api/v1/submissions")
        async def submit() -> dict[str, str | None]:
            job = await pool.enqueue_job("judge", submission_id=7, _queue_name=queue)
            return {"job_id": job.job_id if job else None}

        app.add_middleware(TraceMiddleware)
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
            response = await client.post("/api/v1/submissions")
        assert response.status_code == 200

        worker = Worker(
            functions=[ozy_arq.traced(arq.func(judge, name="judge"))],
            redis_pool=pool,
            queue_name=queue,
            burst=True,
            poll_delay=0.05,
            handle_signals=False,
        )
        await worker.main()
        assert worker.jobs_complete == 1, "the job did not run"
        assert received == {"submission_id": 7}
    finally:
        await pool.flushdb()
        await pool.aclose()

    ozy.tracer.flush()
    spans = {s["name"]: s for s in agent.spans()}
    assert set(spans) >= {"http.request", "arq.enqueue", "arq.job", "sqlite.query"}
    assert len({s["trace_id"] for s in agent.spans()}) == 1  # one trace across both "processes"
    assert spans["arq.enqueue"]["parent_id"] == spans["http.request"]["span_id"]
    assert spans["arq.job"]["parent_id"] == spans["arq.enqueue"]["span_id"]
    assert spans["sqlite.query"]["parent_id"] == spans["arq.job"]["span_id"]
    assert spans["arq.job"]["meta"]["job.id"] == response.json()["job_id"]
    assert spans["arq.job"]["metrics"]["queue.wait_ms"] >= 0
    assert spans["http.request"]["resource"] == "POST /api/v1/submissions"
