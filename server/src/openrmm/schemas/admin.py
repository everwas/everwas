import uuid
from datetime import datetime

from pydantic import BaseModel, ConfigDict, EmailStr, Field

from openrmm.models.user import Role


class UserIn(BaseModel):
    email: EmailStr
    # Long rather than complex. A length floor is the one password rule that
    # measurably helps and does not push people toward Passw0rd!.
    password: str = Field(min_length=12, max_length=256)
    role: Role = Role.viewer


class UserRoleIn(BaseModel):
    role: Role


class UserOut(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    email: str
    role: Role
    is_active: bool
    created_at: datetime


class ApiKeyIn(BaseModel):
    name: str = Field(min_length=1, max_length=120)
    scopes: list[str]
    #: None means it never expires, which is a decision rather than a default.
    ttl_days: int | None = Field(default=365, ge=1, le=3650)


class ApiKeyOut(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    name: str
    key_id: str
    scopes: list[str]
    created_at: datetime
    expires_at: datetime | None
    last_used_at: datetime | None


class ApiKeyCreated(BaseModel):
    """The one response that carries a secret. Only sha256 is stored, so this
    is the only time the key exists in readable form."""

    key: ApiKeyOut
    secret: str


class SiteIn(BaseModel):
    name: str = Field(min_length=1, max_length=120)


class SiteOut(BaseModel):
    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    name: str
    created_at: datetime
