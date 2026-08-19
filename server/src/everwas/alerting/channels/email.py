"""SMTP delivery. Plain text is the payload, HTML is the courtesy."""

import html
from email.message import EmailMessage

import aiosmtplib

from everwas.alerting.channels.base import (
    SEVERITY_COLORS,
    ChannelError,
    Notification,
)
from everwas.config import get_settings

TIMEOUT_S = 10.0


class EmailChannel:
    def __init__(self, config: dict) -> None:
        to = config.get("to") or []
        if isinstance(to, str):
            to = [to]
        recipients = [str(addr).strip() for addr in to if str(addr).strip()]
        if not recipients:
            raise ChannelError("email channel has no recipients", permanent=True)
        self.to = recipients

    async def send(self, note: Notification) -> None:
        settings = get_settings()
        if not settings.smtp_host:
            raise ChannelError("smtp_host is not configured", permanent=True)

        message = build_message(note, self.to, settings.smtp_from)
        try:
            await aiosmtplib.send(
                message,
                hostname=settings.smtp_host,
                port=settings.smtp_port,
                # dev mailpit has no auth; passing empty strings would try AUTH anyway
                username=settings.smtp_user or None,
                password=settings.smtp_password or None,
                timeout=TIMEOUT_S,
            )
        except aiosmtplib.SMTPRecipientsRefused as exc:
            # NOT an SMTPResponseException, so this used to fall through to the
            # generic handler below and classify as transient: five retries
            # over 35 minutes for an address that will never accept mail, while
            # the operator saw "retry scheduled" and assumed it was in hand.
            raise ChannelError(
                f"smtp recipients refused: {_refusals(exc)}", permanent=True
            ) from exc
        except aiosmtplib.SMTPResponseException as exc:
            # 5xx is a refusal, 4xx is a "come back later"
            raise ChannelError(
                f"smtp {exc.code}: {exc.message}", permanent=500 <= exc.code < 600
            ) from exc
        except (aiosmtplib.SMTPException, OSError) as exc:
            raise ChannelError(f"smtp delivery failed: {exc}") from exc


def _refusals(exc: "aiosmtplib.SMTPRecipientsRefused") -> str:
    """Which addresses were refused, and what the server said about each."""
    parts = []
    for refusal in getattr(exc, "recipients", None) or []:
        recipient = getattr(refusal, "recipient", None)
        code = getattr(refusal, "code", None)
        message = getattr(refusal, "message", None)
        parts.append(f"{recipient or '?'} ({code or '?'}: {message or ''})".strip())
    return "; ".join(parts) or str(exc)


def subject_for(note: Notification) -> str:
    return f"[Everwas][{note.severity}] {note.title}"


def build_message(note: Notification, to: list[str], sender: str) -> EmailMessage:
    message = EmailMessage()
    message["From"] = sender
    message["To"] = ", ".join(to)
    message["Subject"] = subject_for(note)
    message["X-Everwas-Kind"] = note.kind
    message["X-Everwas-Severity"] = note.severity
    if note.alert_id:
        message["X-Everwas-Alert"] = note.alert_id
    message.set_content(_text_part(note))
    message.add_alternative(_html_part(note), subtype="html")
    return message


def _detail_rows(note: Notification) -> list[tuple[str, str]]:
    rows: list[tuple[str, str]] = []
    if note.device_hostname:
        rows.append(("Device", note.device_hostname))
    rows.append(("Severity", note.severity))
    rows += [(str(k).replace("_", " "), str(v)) for k, v in note.context.items()]
    return rows


def _text_part(note: Notification) -> str:
    lines = [note.title, "", note.body.strip(), ""]
    lines += [f"{label}: {value}" for label, value in _detail_rows(note)]
    if note.alert_id:
        lines.append(f"alert id: {note.alert_id}")
    return "\n".join(lines).strip() + "\n"


def _html_part(note: Notification) -> str:
    accent = SEVERITY_COLORS.get(note.severity, SEVERITY_COLORS["info"])
    rows = "".join(
        f'<tr><td style="padding:4px 16px 4px 0;color:#61707d;white-space:nowrap">'
        f"{html.escape(label)}</td>"
        f'<td style="padding:4px 0;color:#1f2933">{html.escape(value)}</td></tr>'
        for label, value in _detail_rows(note)
    )
    body = html.escape(note.body.strip()).replace("\n", "<br>")
    footer = (
        f'<p style="margin:20px 0 0;font-size:12px;color:#8c99a6">'
        f"alert {html.escape(note.alert_id)}</p>"
        if note.alert_id
        else ""
    )
    return f"""<!doctype html>
<html><body style="margin:0;padding:24px;background:#f4f6f8;
 font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">
<div style="max-width:560px;margin:0 auto;background:#ffffff;border:1px solid #e4e7eb;
 border-left:4px solid {accent};border-radius:6px;padding:20px 24px">
<p style="margin:0 0 4px;font-size:12px;letter-spacing:.08em;text-transform:uppercase;
 color:{accent}">{html.escape(note.severity)}</p>
<h1 style="margin:0 0 12px;font-size:18px;line-height:1.35;color:#1f2933">
{html.escape(note.title)}</h1>
<p style="margin:0 0 16px;font-size:14px;line-height:1.55;color:#3e4c59">{body}</p>
<table style="border-collapse:collapse;font-size:13px">{rows}</table>
{footer}
</div>
</body></html>
"""
