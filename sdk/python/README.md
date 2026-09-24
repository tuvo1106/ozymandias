# ozy (Python SDK)

Send metrics from any Python 3.12+ app to a [ozymandias](https://github.com/tuvo1106/ozymandias)
agent. The package has no runtime dependencies. It speaks extended StatsD over UDP, and it cannot
raise into your app.

```sh
pip install ozy   # not published yet: see "Install" in the guide below
```

```python
import ozy
from ozy import statsd

ozy.init(service="shop", env="dev", version="1.2.0")  # plus OZY_* env vars

statsd.increment("checkout.completed", tags=["payment:card"])
statsd.gauge("queue.depth", 3)
statsd.distribution("http.request.duration", 12.4, tags=["route:/api/items"])

with statsd.timed("checkout.duration"):
    ...


@statsd.timed("fetch.duration")
async def fetch() -> None: ...
```

With `OZY_AGENT_HOST` unset (and no `agent_host=` argument), every call is a no-op.
The SDK then creates no socket, no thread and no exit hook, so instrumented code is safe to
run anywhere.

The full guide covers configuration, the API reference, formatting and sampling rules,
safety guarantees, fork behaviour and troubleshooting. It lives in the repository at
[`docs/sdk/python.md`](https://github.com/tuvo1106/ozymandias/blob/main/docs/sdk/python.md).

## Development

```sh
uv sync
uv run pytest                 # tests with the 90% coverage gate
uv run ruff check && uv run ruff format --check
uv run mypy                   # strict
uv build                      # wheel + sdist into dist/
```

The contract tests read the shared goldens at `../../pkg/wire/testdata/statsd/sdk-cases.json`,
so run them from a full checkout of the repository.

## License

MIT. See [LICENSE](LICENSE).
