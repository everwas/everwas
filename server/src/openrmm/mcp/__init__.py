"""MCP server: the fleet, exposed to AI assistants as tools.

Off by default (OPENRMM_MCP_ENABLED). Every call is authenticated with a scoped
API key, every call lands in the audit log, and every mutating tool refuses to
act without an explicit confirm.
"""
