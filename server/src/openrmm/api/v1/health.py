from fastapi import APIRouter
from sqlalchemy import text

from openrmm import __version__
from openrmm.api.deps import DbSession

router = APIRouter()


@router.get("/health")
async def health(db: DbSession) -> dict:
    await db.execute(text("SELECT 1"))
    return {"status": "ok", "version": __version__}
