import uuid
from datetime import datetime

from sqlalchemy import DateTime, Integer, LargeBinary, String, Text, func
from sqlalchemy.orm import Mapped, mapped_column

from openrmm.db.base import Base


class IngestDeadLetter(Base):
    """A message that could not be processed after repeated attempts.

    Without this, `max_deliver` silently discards poison messages and the only
    trace is a log line that scrolled past. Parking the raw payload and the
    traceback means an operator can answer "what did we lose, and why".
    """

    __tablename__ = "ingest_dead_letter"

    id: Mapped[uuid.UUID] = mapped_column(primary_key=True, default=uuid.uuid4)
    at: Mapped[datetime] = mapped_column(DateTime(timezone=True), server_default=func.now())
    stream: Mapped[str] = mapped_column(String(64))
    durable: Mapped[str] = mapped_column(String(64))
    subject: Mapped[str] = mapped_column(String(255), index=True)
    payload: Mapped[bytes] = mapped_column(LargeBinary)
    delivered: Mapped[int] = mapped_column(Integer, default=0)
    error: Mapped[str] = mapped_column(Text, default="")
