FROM ghcr.io/astral-sh/uv:python3.13-bookworm-slim AS base

ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    PYTHONUNBUFFERED=1

WORKDIR /app

# Dependencies first (cached layer), project second.
RUN --mount=type=cache,target=/root/.cache/uv \
    --mount=type=bind,source=server/uv.lock,target=uv.lock \
    --mount=type=bind,source=server/pyproject.toml,target=pyproject.toml \
    uv sync --frozen --no-install-project --no-dev

COPY server/pyproject.toml server/uv.lock server/README.md ./
COPY server/src ./src
COPY server/alembic.ini ./
COPY server/alembic ./alembic

ENV PATH="/app/.venv/bin:$PATH"

# --- dev: editable install + dev deps, source bind-mounted by compose ---
FROM base AS dev
RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen
CMD ["uvicorn", "everwas.api.app:app", "--host", "0.0.0.0", "--port", "8000", "--reload"]

# --- prod: non-editable, no dev deps, non-root ---
FROM base AS prod
RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-dev --no-editable
RUN useradd -r -u 10001 everwas && chown -R everwas /app
USER everwas
CMD ["uvicorn", "everwas.api.app:app", "--host", "0.0.0.0", "--port", "8000"]
