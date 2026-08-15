from fastapi import APIRouter
from sqlalchemy import select

from openrmm.api.deps import CurrentUser, DbSession
from openrmm.models.device import Device
from openrmm.schemas.device import DeviceOut

router = APIRouter()


@router.get("")
async def list_devices(db: DbSession, _user: CurrentUser) -> list[DeviceOut]:
    rows = await db.execute(select(Device).order_by(Device.hostname))
    return [DeviceOut.model_validate(d) for d in rows.scalars()]
