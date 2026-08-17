"""Walk every route the app actually serves.

`for route in app.routes` does NOT find them on this FastAPI version: routers
included with a prefix appear as `_IncludedRouter` wrappers whose children live
in `.routes`, so a flat `isinstance(route, APIRoute)` loop matches nothing at
all.

Two tests were built on that loop. `test_every_route_requires_auth_except_the_
documented_few` iterated zero routes and therefore passed unconditionally,
whatever the code did, while reading as though it proved authentication on the
whole API. That is the third test in this review found to be incapable of
failing, and the one guarding the widest surface.
"""

from fastapi.routing import APIRoute, APIWebSocketRoute


def api_routes(app, *, websockets: bool = False):
    """Every APIRoute in the app, descending into included routers.

    websockets=True also yields APIWebSocketRoute. They are worth including
    for authorization checks: the shell WebSocket authenticates by hand inside
    its handler, which is exactly the kind of route a conformance test should
    cover rather than skip.
    """
    wanted = (APIRoute, APIWebSocketRoute) if websockets else (APIRoute,)

    def walk(routes, prefix=""):
        for route in routes:
            if isinstance(route, wanted):
                # Paths inside an included router are RELATIVE: the prefix
                # lives on the include context, not on the route. Rebuilding
                # the full path here matters as much as finding the route,
                # because the smoke test compares against "/api/v1/health".
                yield _Prefixed(route, prefix + route.path)
                continue
            included = getattr(route, "original_router", None)
            if included is not None:
                ctx = getattr(route, "include_context", None)
                yield from walk(included.routes, prefix + getattr(ctx, "prefix", ""))
                continue
            children = getattr(route, "routes", None)
            if children:
                yield from walk(children, prefix)

    return list(walk(app.routes))


class _Prefixed:
    """A route with its full, mounted path.

    Wraps rather than mutates: the route objects belong to a live app instance
    and rewriting .path on them would change routing for anything sharing it.
    """

    __slots__ = ("route", "path")

    def __init__(self, route, path):
        self.route = route
        self.path = path

    def __getattr__(self, name):
        return getattr(self.route, name)

    def __repr__(self):
        return f"<Route {sorted(getattr(self.route, 'methods', []) or [])} {self.path}>"
