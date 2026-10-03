"""Framework integrations: small adapters from a web framework to :mod:`ozy`.

Each integration is built only on the public ``ozy.statsd`` API, so anything
shipped here is something an app could write itself. Importing one never
imports the framework it targets: the ASGI middleware speaks the ASGI spec,
not Starlette's classes, which is how the SDK keeps zero dependencies.
"""
