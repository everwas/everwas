"""Scoping queries to the caller's organization.

The boundary is enforced here rather than spelled out at each route, because
"remember to add the filter" is the failure mode this codebase keeps finding:
a rule applied correctly in one file and not in its sibling.

Two rules, and the second is the one that matters:

Scope by adding a predicate, never by trusting the caller to compare ids after
the fact. A route that loads a row and then checks it has already leaked its
existence through timing and through 403-versus-404.

Fail CLOSED for a caller with no organization. The obvious spelling,
`WHERE org_id = :caller`, silently matches nothing when :caller is NULL, which
happens to be right. The dangerous spelling is the one someone writes to "fix"
that: skipping the filter when the caller has no org turns a broken user into a
superuser across every tenant. `no_rows()` exists so the fail-closed case is
explicit rather than an accident of SQL NULL semantics.

A stronger version of this would be Postgres row-level security, which cannot
be forgotten at all. It is not used here because ingest and the dispatcher write
agent-sourced rows with no user context and would need a bypass, which
reintroduces a hole in a less visible place. The enumeration test in
tests/test_org_isolation.py is the substitute: it reads device-scoped routes off
the live app, so a new route is covered the day it is added.
"""

import uuid
from typing import TYPE_CHECKING

from sqlalchemy import Select, false

if TYPE_CHECKING:  # pragma: no cover
    from openrmm.models.user import User


def caller_org(user: "User | None") -> uuid.UUID | None:
    """The organization a request acts within, or None if it has none."""
    return getattr(user, "org_id", None) if user is not None else None


def no_rows(query: Select) -> Select:
    """Make a query match nothing, explicitly.

    Used for a caller with no organization. Spelled out rather than relying on
    `org_id = NULL` being false, so the intent survives someone later
    "simplifying" the filter.
    """
    return query.where(false())


def scope_to_org(query: Select, column, org_id: uuid.UUID | None) -> Select:
    """Restrict a query to one organization, or to nothing."""
    if org_id is None:
        return no_rows(query)
    return query.where(column == org_id)


def in_org(row, org_id: uuid.UUID | None) -> bool:
    """Whether a loaded row belongs to the caller's organization.

    For the handful of places that already hold a row. Prefer scope_to_org:
    filtering in the query means a foreign row is never loaded, never logged,
    and cannot be distinguished from a nonexistent one by response timing.
    """
    if org_id is None or row is None:
        return False
    return getattr(row, "org_id", None) == org_id
