import uuid

from pydantic import BaseModel, EmailStr

from openrmm.models.user import Role


class LoginRequest(BaseModel):
    email: EmailStr
    password: str


class UserOut(BaseModel):
    id: uuid.UUID
    email: str
    role: Role

    model_config = {"from_attributes": True}
