"""Tool registration.

Tools live in two modules so the split is visible in the file tree: reading the
fleet, and changing it. Registration is explicit rather than import-time
side effects, which keeps `everwas.mcp.server` importable in any order.
"""

from fastmcp import FastMCP

from everwas.mcp import tools_actions, tools_fleet


def register_tools(mcp: FastMCP) -> None:
    tools_fleet.register(mcp)
    tools_actions.register(mcp)
