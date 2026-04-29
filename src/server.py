#!/usr/bin/env python3
__version__ = "0.1.0"

import asyncio
import logging
import os
import smtplib
import ssl
import time
from contextlib import asynccontextmanager
from datetime import date
from email import policy
from email.mime.multipart import MIMEMultipart
from email.mime.text import MIMEText
from email.parser import BytesParser
from typing import Optional

import hmac

import httpx
import uvicorn
import yaml
from imapclient import IMAPClient
from fastmcp import FastMCP, Context
from fastmcp.server.auth import TokenVerifier, AccessToken
from starlette.middleware import Middleware
from starlette.responses import JSONResponse, Response
from starlette.types import ASGIApp, Receive, Scope, Send

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
logger = logging.getLogger("poke-mail")
logging.getLogger("httpx").setLevel(logging.WARNING)


# ---------------------------------------------------------------------------
# Auth — simple bearer token verification
# ---------------------------------------------------------------------------


class ApiKeyAuth(TokenVerifier):
    """Validates incoming requests against a static API key (MCP_API_KEY)."""

    def __init__(self, api_key: str):
        super().__init__()
        self._api_key = api_key

    async def verify_token(self, token: str) -> AccessToken | None:
        if hmac.compare_digest(token, self._api_key):
            return AccessToken(token=token, client_id="owner", scopes=["all"])
        return None


# ---------------------------------------------------------------------------
# Route filter — silently drop non-MCP requests
# ---------------------------------------------------------------------------


class DropNonMCPRoutes:
    """Return empty 404 for any path outside /mcp — reveals nothing to scanners."""

    def __init__(self, app: ASGIApp):
        self.app = app

    async def __call__(self, scope: Scope, receive: Receive, send: Send):
        if scope["type"] == "http" and not scope["path"].startswith("/mcp"):
            response = Response(status_code=404)
            await response(scope, receive, send)
            return
        await self.app(scope, receive, send)


class RateLimitMiddleware:
    """Per-IP sliding window rate limiter with separate buckets for GET and POST.

    GET /mcp (health/polling) gets a tighter limit to curb excessive polling.
    POST /mcp (tool calls) gets a higher limit so real work isn't blocked.
    """

    MAX_TRACKED_IPS = 1024

    def __init__(self, app: ASGIApp):
        self.app = app
        self.get_rpm = int(os.environ.get("RATE_LIMIT_GET_RPM", "30"))
        self.post_rpm = int(os.environ.get("RATE_LIMIT_POST_RPM", "120"))
        self.window = 60  # seconds
        self._hits: dict[str, list[float]] = {}
        self._last_cleanup = time.monotonic()

    def _client_ip(self, scope: Scope) -> str:
        # Only trust X-Forwarded-For from known proxy — take the rightmost
        # entry (closest to our server) to resist spoofing via prepended IPs.
        for header_name, header_val in scope.get("headers", []):
            if header_name == b"x-forwarded-for":
                parts = header_val.decode().split(",")
                return parts[-1].strip()
        client = scope.get("client")
        return client[0] if client else "unknown"

    def _cleanup_stale(self, now: float) -> None:
        """Periodically evict stale IPs to bound memory usage."""
        if now - self._last_cleanup < self.window:
            return
        self._last_cleanup = now
        cutoff = now - self.window
        stale = [ip for ip, ts in self._hits.items() if not ts or ts[-1] <= cutoff]
        for ip in stale:
            del self._hits[ip]
        # Hard cap: if still too many, drop the oldest entries
        if len(self._hits) > self.MAX_TRACKED_IPS:
            by_recency = sorted(self._hits, key=lambda ip: self._hits[ip][-1])
            for ip in by_recency[: len(self._hits) - self.MAX_TRACKED_IPS]:
                del self._hits[ip]

    def _is_limited(self, bucket: str, rpm: int) -> tuple[bool, int]:
        now = time.monotonic()
        self._cleanup_stale(now)

        timestamps = self._hits.get(bucket, [])
        cutoff = now - self.window
        timestamps = [t for t in timestamps if t > cutoff]
        self._hits[bucket] = timestamps

        if len(timestamps) >= rpm:
            oldest = timestamps[0]
            retry_after = int(oldest + self.window - now) + 1
            return True, max(retry_after, 1)

        timestamps.append(now)
        return False, 0

    async def __call__(self, scope: Scope, receive: Receive, send: Send):
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        ip = self._client_ip(scope)
        method = scope.get("method", "GET")
        if method == "POST":
            bucket, rpm = f"{ip}:post", self.post_rpm
        else:
            bucket, rpm = f"{ip}:get", self.get_rpm

        limited, retry_after = self._is_limited(bucket, rpm)
        if limited:
            response = JSONResponse(
                {"error": "rate_limited", "retry_after": retry_after},
                status_code=429,
                headers={"Retry-After": str(retry_after)},
            )
            await response(scope, receive, send)
            return
        await self.app(scope, receive, send)


# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------


def load_config() -> dict:
    path = os.environ.get("CONFIG_PATH", "config.yml")
    try:
        with open(path) as f:
            return yaml.safe_load(f) or {}
    except FileNotFoundError:
        logger.warning("Config file %s not found, using env vars", path)
        return {}


def parse_accounts(config: dict) -> list[dict]:
    accounts = config.get("accounts", [])
    if not accounts:
        # Fallback to flat env vars for single account
        imap_host = os.environ.get("IMAP_HOST")
        if not imap_host:
            raise RuntimeError(
                "No accounts configured. Set POKE_MAIL_ACCOUNTS env var or create config.yml"
            )
        accounts = [
            {
                "id": "default",
                "imap_host": imap_host,
                "imap_port": int(os.environ.get("IMAP_PORT", "993")),
                "imap_username": os.environ["IMAP_USERNAME"],
                "imap_password": os.environ["IMAP_PASSWORD"],
                "smtp_host": os.environ.get("SMTP_HOST", imap_host),
                "smtp_port": int(os.environ.get("SMTP_PORT", "587")),
                "smtp_username": os.environ.get(
                    "SMTP_USERNAME", os.environ["IMAP_USERNAME"]
                ),
                "smtp_password": os.environ.get(
                    "SMTP_PASSWORD", os.environ["IMAP_PASSWORD"]
                ),
                "from_address": os.environ.get(
                    "FROM_ADDRESS",
                    os.environ.get("SMTP_USERNAME", os.environ["IMAP_USERNAME"]),
                ),
                "watch_folders": ["INBOX"],
                "mark_as_read": os.environ.get("MARK_AS_READ", "false").lower()
                == "true",
            }
        ]

    global_allow_send = config.get("allow_send", True)
    global_mark_as_read = config.get("mark_as_read", False)
    required = ("imap_host", "imap_username", "imap_password")
    for i, acc in enumerate(accounts):
        acc.setdefault("id", f"account-{i}")
        acc.setdefault("imap_port", 993)
        acc.setdefault("watch_folders", ["INBOX"])
        # SMTP falls back to IMAP if not specified
        acc.setdefault("smtp_host", acc.get("imap_host"))
        acc.setdefault("smtp_port", 587)
        acc.setdefault("smtp_username", acc.get("imap_username"))
        acc.setdefault("smtp_password", acc.get("imap_password"))
        acc.setdefault("from_address", acc.get("smtp_username"))
        acc.setdefault("allow_send", global_allow_send)
        acc.setdefault("mark_as_read", global_mark_as_read)
        for field in required:
            if field not in acc:
                raise RuntimeError(
                    f"Account '{acc['id']}' missing required field: {field}"
                )
    return accounts


def resolve_account(accounts: list[dict], account_id: str) -> dict:
    if not account_id or not account_id.strip():
        raise ValueError(
            f"account_id is required. Available accounts: {[a['id'] for a in accounts]}. "
            f"Call list_accounts to see all inboxes."
        )
    needle = account_id.strip().lower()
    # Exact id match (case-insensitive, whitespace-tolerant)
    for acc in accounts:
        if acc["id"].lower() == needle:
            return acc
    # Fallback: match by email address (from_address, imap_username, smtp_username).
    # Collect all matches so we can detect ambiguity instead of silently picking the first.
    matches = [
        acc
        for acc in accounts
        if needle
        in {
            (acc.get("from_address") or "").lower(),
            (acc.get("imap_username") or "").lower(),
            (acc.get("smtp_username") or "").lower(),
        }
    ]
    if len(matches) == 1:
        return matches[0]
    if len(matches) > 1:
        raise ValueError(
            f"account_id '{account_id}' matches multiple accounts: "
            f"{[a['id'] for a in matches]}. Pass the unique 'id' instead."
        )
    raise ValueError(
        f"Unknown account_id '{account_id}'. Available: {[a['id'] for a in accounts]}. "
        f"Call list_accounts for full details."
    )


# ---------------------------------------------------------------------------
# IMAP helpers
# ---------------------------------------------------------------------------


# RFC 2971 IMAP ID — required by some providers (e.g. netease 163.com / 126.com / yeah.net)
# which reject clients that don't identify themselves after login.
_IMAP_CLIENT_ID = {
    "name": "poke-mail",
    "version": "1.0.0",
    "vendor": "Poke Interactions",
}


def get_imap_client(account: dict) -> IMAPClient:
    port = account["imap_port"]
    use_ssl = port == 993
    client = IMAPClient(account["imap_host"], port=port, ssl=use_ssl)
    if not use_ssl:
        client.starttls()
    client.login(account["imap_username"], account["imap_password"])
    # Send IMAP ID if the server supports it. Non-fatal: not all servers do,
    # and we never want this to break an otherwise-working login.
    try:
        if client.has_capability("ID"):
            client.id_(_IMAP_CLIENT_ID)
    except Exception as e:
        logger.debug("IMAP ID command failed for %s: %s", account.get("id"), e)
    return client


def parse_email_message(raw: bytes) -> dict:
    msg = BytesParser(policy=policy.default).parsebytes(raw)

    body_text = ""
    body_html = ""
    attachments = []

    if msg.is_multipart():
        for part in msg.walk():
            ct = part.get_content_type()
            cd = str(part.get("Content-Disposition", ""))
            if "attachment" in cd:
                try:
                    content = part.get_content()
                    size = len(content) if hasattr(content, "__len__") else 0
                except Exception:
                    size = 0
                attachments.append(
                    {
                        "filename": part.get_filename() or "unnamed",
                        "content_type": ct,
                        "size": size,
                    }
                )
            elif ct == "text/plain" and not body_text:
                try:
                    body_text = part.get_content()
                except Exception:
                    body_text = part.get_payload(decode=True).decode(errors="replace")
            elif ct == "text/html" and not body_html:
                try:
                    body_html = part.get_content()
                except Exception:
                    body_html = part.get_payload(decode=True).decode(errors="replace")
    else:
        ct = msg.get_content_type()
        try:
            content = msg.get_content()
        except Exception:
            payload = msg.get_payload(decode=True)
            content = payload.decode(errors="replace") if payload else ""
        if ct == "text/html":
            body_html = content
        else:
            body_text = content

    to_header = msg["to"] or ""
    cc_header = msg["cc"] or ""

    def parse_addresses(header):
        if not header:
            return []
        return [addr.strip() for addr in str(header).split(",") if addr.strip()]

    return {
        "from": str(msg["from"] or ""),
        "to": parse_addresses(to_header),
        "cc": parse_addresses(cc_header),
        "subject": str(msg["subject"] or ""),
        "date": str(msg["date"] or ""),
        "body_text": body_text,
        "body_html": body_html,
        "headers": {k: str(v) for k, v in msg.items()},
        "attachments": attachments,
    }


def build_search_criteria(
    from_addr: Optional[str] = None,
    to_addr: Optional[str] = None,
    subject: Optional[str] = None,
    since: Optional[str] = None,
    before: Optional[str] = None,
) -> list:
    criteria = []
    if from_addr:
        criteria.extend(["FROM", from_addr])
    if to_addr:
        criteria.extend(["TO", to_addr])
    if subject:
        criteria.extend(["SUBJECT", subject])
    if since:
        criteria.extend(["SINCE", date.fromisoformat(since)])
    if before:
        criteria.extend(["BEFORE", date.fromisoformat(before)])
    if not criteria:
        criteria = ["ALL"]
    return criteria


def detect_archive_folder(client: IMAPClient) -> str:
    folders = client.list_folders()
    for flags, _delim, name in folders:
        if b"\\Archive" in flags:
            return name
        if name in ("[Gmail]/All Mail", "Archive"):
            return name
    return "Archive"


def detect_drafts_folder(client: IMAPClient) -> str:
    folders = client.list_folders()
    for flags, _delim, name in folders:
        if b"\\Drafts" in flags:
            return name
    for name in ("Drafts", "[Gmail]/Drafts", "INBOX.Drafts"):
        if client.folder_exists(name):
            return name
    return "Drafts"


# ---------------------------------------------------------------------------
# Poke webhook
# ---------------------------------------------------------------------------


async def forward_to_poke(
    email_data: dict, account: dict, webhook_url: str, api_key: str
) -> bool:
    payload = {
        "account_id": account["id"],
        "from_address": account["from_address"],
        "from": email_data["from"],
        "to": email_data["to"],
        "subject": email_data["subject"],
        "date": email_data["date"],
        "body_text": email_data["body_text"],
        "body_html": email_data["body_html"],
        "headers": email_data.get("headers", {}),
        "attachments": email_data.get("attachments", []),
    }
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    else:
        logger.warning(
            "No Poke API key configured — webhook request will be unauthenticated"
        )

    logger.debug(
        "Forwarding to %s (api_key set: %s, key prefix: %s)",
        webhook_url,
        bool(api_key),
        api_key[:8] + "..." if api_key and len(api_key) > 8 else "***",
    )

    for attempt in range(2):
        try:
            async with httpx.AsyncClient(timeout=30) as http:
                resp = await http.post(webhook_url, json=payload, headers=headers)
                resp.raise_for_status()
                logger.info(
                    "Forwarded email '%s' to Poke (status %d)",
                    email_data["subject"],
                    resp.status_code,
                )
                return True
        except Exception as e:
            logger.warning("Forward attempt %d failed: %s", attempt + 1, e)
            if attempt == 0:
                await asyncio.sleep(2)
    return False


# ---------------------------------------------------------------------------
# IDLE watcher
# ---------------------------------------------------------------------------


async def watch_folder(
    account: dict,
    folder: str,
    webhook_url: str,
    api_key: str,
    stop_event: asyncio.Event,
):
    backoff = 5
    max_backoff = 60

    while not stop_event.is_set():
        client = None
        try:
            client = await asyncio.to_thread(get_imap_client, account)
            await asyncio.to_thread(
                client.select_folder,
                folder,
                readonly=not account.get("mark_as_read", False),
            )

            # Check IDLE support
            if not client.has_capability("IDLE"):
                logger.warning(
                    "[%s/%s] Server does not support IDLE, falling back to polling",
                    account["id"],
                    folder,
                )
                await _poll_folder(
                    client, account, folder, webhook_url, api_key, stop_event
                )
                return

            # Record existing unseen UIDs so we only forward truly new ones
            existing_unseen = set(await asyncio.to_thread(client.search, ["UNSEEN"]))
            logger.info(
                "[%s/%s] Watching for new emails via IDLE (%d existing unseen skipped)",
                account["id"],
                folder,
                len(existing_unseen),
            )
            backoff = 5

            while not stop_event.is_set():
                await asyncio.to_thread(client.idle)
                try:
                    responses = await asyncio.to_thread(client.idle_check, 120)
                except Exception:
                    try:
                        await asyncio.to_thread(client.idle_done)
                    except Exception:
                        pass
                    break
                await asyncio.to_thread(client.idle_done)

                logger.debug(
                    "[%s/%s] IDLE responses: %s", account["id"], folder, responses
                )

                uids = await asyncio.to_thread(client.search, ["UNSEEN"])
                # Only forward emails that arrived after we started watching
                new_uids = [u for u in uids if u not in existing_unseen]
                if not new_uids:
                    continue

                raw_messages = await asyncio.to_thread(
                    client.fetch, new_uids, ["RFC822"]
                )
                for uid, data in raw_messages.items():
                    raw = data.get(b"RFC822", b"")
                    if not raw:
                        continue
                    email_data = parse_email_message(raw)
                    await forward_to_poke(email_data, account, webhook_url, api_key)

                if account.get("mark_as_read", False):
                    await asyncio.to_thread(client.set_flags, new_uids, [b"\\Seen"])
                existing_unseen.update(new_uids)

        except asyncio.CancelledError:
            break
        except Exception as e:
            logger.error(
                "[%s/%s] Watcher error: %s (reconnecting in %ds)",
                account["id"],
                folder,
                e,
                backoff,
            )
            await asyncio.sleep(backoff)
            backoff = min(backoff * 2, max_backoff)
        finally:
            if client:
                try:
                    await asyncio.to_thread(client.logout)
                except Exception:
                    pass


async def _poll_folder(
    client: IMAPClient,
    account: dict,
    folder: str,
    webhook_url: str,
    api_key: str,
    stop_event: asyncio.Event,
):
    """Fallback polling for servers without IDLE support. Checks every 60 seconds."""
    logger.info("[%s/%s] Polling for new emails every 60s", account["id"], folder)
    while not stop_event.is_set():
        try:
            uids = await asyncio.to_thread(client.search, ["UNSEEN"])
            if uids:
                raw_messages = await asyncio.to_thread(client.fetch, uids, ["RFC822"])
                for uid, data in raw_messages.items():
                    raw = data.get(b"RFC822", b"")
                    if not raw:
                        continue
                    email_data = parse_email_message(raw)
                    await forward_to_poke(email_data, account, webhook_url, api_key)
                if account.get("mark_as_read", False):
                    await asyncio.to_thread(client.set_flags, uids, [b"\\Seen"])
        except Exception as e:
            logger.warning("[%s/%s] Poll error: %s", account["id"], folder, e)
            raise  # reconnect via outer loop
        await asyncio.sleep(60)


async def idle_watcher(
    accounts: list[dict], webhook_url: str, api_key: str, stop_event: asyncio.Event
):
    tasks = []
    for acc in accounts:
        for folder in acc.get("watch_folders", ["INBOX"]):
            tasks.append(
                asyncio.create_task(
                    watch_folder(acc, folder, webhook_url, api_key, stop_event)
                )
            )
    if tasks:
        await asyncio.gather(*tasks, return_exceptions=True)


# ---------------------------------------------------------------------------
# Lifespan
# ---------------------------------------------------------------------------


@asynccontextmanager
async def lifespan(server: FastMCP):
    config = load_config()
    accounts = parse_accounts(config)
    webhook_url = os.environ.get(
        "POKE_WEBHOOK_URL",
        config.get("webhook_url", "https://poke.com/api/v1/inbound/api-message"),
    )
    api_key = os.environ.get("POKE_API_KEY", config.get("poke_api_key", ""))
    stop_event = asyncio.Event()

    watcher_task = asyncio.create_task(
        idle_watcher(accounts, webhook_url, api_key, stop_event)
    )
    logger.info("poke-mail started with %d account(s)", len(accounts))

    try:
        yield {
            "accounts": accounts,
            "webhook_url": webhook_url,
            "api_key": api_key,
            "config": config,
        }
    finally:
        stop_event.set()
        watcher_task.cancel()
        try:
            await watcher_task
        except asyncio.CancelledError:
            pass
        logger.info("poke-mail shut down")


# ---------------------------------------------------------------------------
# MCP Server & Tools
# ---------------------------------------------------------------------------

mcp_api_key = os.environ.get("MCP_API_KEY", "")

# When running behind the poke tunnel (POKE_TUNNEL=1), the tunnel handles
# authentication so the MCP_API_KEY bearer check is optional.
# In direct / Docker deployments the key is still required for security.
poke_tunnel_mode = os.environ.get("POKE_TUNNEL", "") == "1"

if mcp_api_key:
    auth = ApiKeyAuth(mcp_api_key)
elif poke_tunnel_mode:
    auth = None  # tunnel handles auth
    logger.info(
        "POKE_TUNNEL=1 detected — MCP_API_KEY not required (tunnel handles auth)."
    )
else:
    auth = None
    logger.warning(
        "MCP_API_KEY not set — server is unauthenticated. "
        "Set MCP_API_KEY or use POKE_TUNNEL=1 to silence this warning."
    )

mcp = FastMCP("poke-mail", lifespan=lifespan, auth=auth)


@mcp.custom_route("/mcp", methods=["GET"])
async def health(request):
    return JSONResponse({"status": "ok"})


@mcp.tool(
    description="Search emails by criteria. Returns a list of matching emails with metadata. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def search_emails(
    ctx: Context,
    account_id: str,
    folder: str = "INBOX",
    from_addr: Optional[str] = None,
    to_addr: Optional[str] = None,
    subject: Optional[str] = None,
    since: Optional[str] = None,
    before: Optional[str] = None,
    limit: int = 20,
) -> list[dict]:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)
    criteria = build_search_criteria(from_addr, to_addr, subject, since, before)

    def _search():
        client = get_imap_client(acc)
        try:
            client.select_folder(folder, readonly=True)
            uids = client.search(criteria)
            uids = uids[-limit:]  # most recent
            if not uids:
                return []
            data = client.fetch(uids, ["ENVELOPE", "FLAGS", "RFC822.SIZE"])
            results = []
            for uid, msg_data in data.items():
                env = msg_data.get(b"ENVELOPE")
                if not env:
                    continue

                def _fmt_addr(addr):
                    """Format an IMAP envelope address safely."""
                    try:
                        name = addr.name.decode(errors="replace") if addr.name else ""
                        mailbox = (
                            addr.mailbox.decode(errors="replace")
                            if addr.mailbox
                            else ""
                        )
                        host = addr.host.decode(errors="replace") if addr.host else ""
                        email = f"{mailbox}@{host}" if mailbox else ""
                        return f"{name} <{email}>" if name else email
                    except Exception:
                        return str(addr)

                results.append(
                    {
                        "uid": uid,
                        "from": _fmt_addr(env.from_[0]) if env.from_ else "",
                        "to": [_fmt_addr(a) for a in (env.to or [])],
                        "subject": env.subject.decode(errors="replace")
                        if env.subject
                        else "",
                        "date": str(env.date) if env.date else "",
                        "flags": [
                            f.decode(errors="replace")
                            for f in msg_data.get(b"FLAGS", [])
                        ],
                        "size": msg_data.get(b"RFC822.SIZE", 0),
                    }
                )
            return results
        finally:
            client.logout()

    return await asyncio.to_thread(_search)


@mcp.tool(
    description="Read a specific email by UID. Returns full email content including body and attachment metadata. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def read_email(
    ctx: Context,
    account_id: str,
    uid: int,
    folder: str = "INBOX",
) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    def _read():
        client = get_imap_client(acc)
        try:
            client.select_folder(folder, readonly=True)
            data = client.fetch([uid], ["RFC822"])
            if uid not in data:
                return {"error": f"Email UID {uid} not found in {folder}"}
            return parse_email_message(data[uid][b"RFC822"])
        finally:
            client.logout()

    return await asyncio.to_thread(_read)


@mcp.tool(
    description="Send an email via SMTP. Supports plain text and HTML, CC/BCC, and reply threading. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def send_email(
    ctx: Context,
    account_id: str,
    to: str,
    subject: str,
    body: str,
    cc: Optional[str] = None,
    bcc: Optional[str] = None,
    html: Optional[str] = None,
    reply_to_uid: Optional[int] = None,
    reply_to_folder: Optional[str] = None,
) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    if not acc.get("allow_send", True):
        return {
            "error": f"Sending is disabled for account '{acc['id']}'. Use create_draft instead."
        }

    def _send():
        msg = MIMEMultipart("alternative") if html else MIMEText(body)
        if html:
            msg.attach(MIMEText(body, "plain"))
            msg.attach(MIMEText(html, "html"))

        msg["From"] = acc["from_address"]
        msg["To"] = to
        msg["Subject"] = subject
        if cc:
            msg["Cc"] = cc

        # Threading headers for replies
        if reply_to_uid and reply_to_folder:
            imap = None
            try:
                imap = get_imap_client(acc)
                imap.select_folder(reply_to_folder or "INBOX", readonly=True)
                orig_data = imap.fetch([reply_to_uid], ["RFC822.HEADER"])
                if reply_to_uid in orig_data:
                    orig = BytesParser(policy=policy.default).parsebytes(
                        orig_data[reply_to_uid][b"RFC822.HEADER"]
                    )
                    if orig["Message-ID"]:
                        msg["In-Reply-To"] = orig["Message-ID"]
                        refs = orig.get("References", "")
                        msg["References"] = f"{refs} {orig['Message-ID']}".strip()
            except Exception as e:
                logger.warning("Could not fetch reply headers: %s", e)
            finally:
                if imap:
                    try:
                        imap.logout()
                    except Exception:
                        pass

        recipients = [addr.strip() for addr in to.split(",")]
        if cc:
            recipients.extend(addr.strip() for addr in cc.split(","))
        if bcc:
            recipients.extend(addr.strip() for addr in bcc.split(","))

        port = acc["smtp_port"]
        if port == 465:
            ctx_ssl = ssl.create_default_context()
            with smtplib.SMTP_SSL(acc["smtp_host"], port, context=ctx_ssl) as smtp:
                smtp.login(acc["smtp_username"], acc["smtp_password"])
                smtp.sendmail(acc["smtp_username"], recipients, msg.as_string())
        else:
            with smtplib.SMTP(acc["smtp_host"], port) as smtp:
                smtp.starttls()
                smtp.login(acc["smtp_username"], acc["smtp_password"])
                smtp.sendmail(acc["smtp_username"], recipients, msg.as_string())

        return {"success": True, "message_id": msg.get("Message-ID", "")}

    return await asyncio.to_thread(_send)


@mcp.tool(
    description="Save an email as a draft for review before sending. The draft appears in the account's Drafts folder. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def create_draft(
    ctx: Context,
    account_id: str,
    to: str,
    subject: str,
    body: str,
    cc: Optional[str] = None,
    bcc: Optional[str] = None,
    html: Optional[str] = None,
) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    def _draft():
        msg = MIMEMultipart("alternative") if html else MIMEText(body)
        if html:
            msg.attach(MIMEText(body, "plain"))
            msg.attach(MIMEText(html, "html"))

        msg["From"] = acc["from_address"]
        msg["To"] = to
        msg["Subject"] = subject
        if cc:
            msg["Cc"] = cc
        if bcc:
            msg["Bcc"] = bcc

        client = get_imap_client(acc)
        try:
            drafts_folder = detect_drafts_folder(client)
            if not client.folder_exists(drafts_folder):
                client.create_folder(drafts_folder)
            client.append(drafts_folder, msg.as_bytes(), flags=[b"\\Draft", b"\\Seen"])
            return {"success": True, "folder": drafts_folder}
        finally:
            client.logout()

    return await asyncio.to_thread(_draft)


@mcp.tool(
    description="Archive an email by moving it to the Archive folder instead of deleting. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def archive_email(
    ctx: Context,
    account_id: str,
    uid: int,
    folder: str = "INBOX",
) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    def _archive():
        client = get_imap_client(acc)
        try:
            client.select_folder(folder)
            archive_folder = detect_archive_folder(client)
            # Ensure archive folder exists
            if not client.folder_exists(archive_folder):
                client.create_folder(archive_folder)
            client.copy([uid], archive_folder)
            client.delete_messages([uid])
            client.expunge()
            return {"success": True, "archived_to": archive_folder}
        finally:
            client.logout()

    return await asyncio.to_thread(_archive)


@mcp.tool(
    description="Move an email from one folder to another. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def move_email(
    ctx: Context,
    account_id: str,
    uid: int,
    to_folder: str,
    from_folder: str = "INBOX",
) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    def _move():
        client = get_imap_client(acc)
        try:
            client.select_folder(from_folder)
            client.copy([uid], to_folder)
            client.delete_messages([uid])
            client.expunge()
            return {"success": True, "moved_to": to_folder}
        finally:
            client.logout()

    return await asyncio.to_thread(_move)


@mcp.tool(
    description="Mark an email as read, unread, flagged, or unflagged. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def mark_email(
    ctx: Context,
    account_id: str,
    uid: int,
    action: str,
    folder: str = "INBOX",
) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    flag_map = {
        "read": (b"\\Seen", "add"),
        "unread": (b"\\Seen", "remove"),
        "flagged": (b"\\Flagged", "add"),
        "unflagged": (b"\\Flagged", "remove"),
    }
    if action not in flag_map:
        return {
            "error": f"Invalid action: {action}. Use: read, unread, flagged, unflagged"
        }

    flag, op = flag_map[action]

    def _mark():
        client = get_imap_client(acc)
        try:
            client.select_folder(folder)
            if op == "add":
                client.add_flags([uid], [flag])
            else:
                client.remove_flags([uid], [flag])
            return {"success": True}
        finally:
            client.logout()

    return await asyncio.to_thread(_mark)


@mcp.tool(
    description="List all IMAP folders for an account. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def list_folders(
    ctx: Context,
    account_id: str,
) -> list[dict]:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    def _list():
        client = get_imap_client(acc)
        try:
            folders = client.list_folders()
            return [
                {
                    "name": name,
                    "flags": [f.decode(errors="replace") for f in flags],
                    "delimiter": delim.decode(errors="replace") if delim else "/",
                }
                for flags, delim, name in folders
            ]
        finally:
            client.logout()

    return await asyncio.to_thread(_list)


@mcp.tool(
    description="Create a new IMAP folder. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def create_folder(
    ctx: Context,
    account_id: str,
    name: str,
) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    def _create():
        client = get_imap_client(acc)
        try:
            client.create_folder(name)
            return {"success": True}
        finally:
            client.logout()

    return await asyncio.to_thread(_create)


@mcp.tool(
    description="Rename an existing IMAP folder. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def rename_folder(
    ctx: Context,
    account_id: str,
    old_name: str,
    new_name: str,
) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    def _rename():
        client = get_imap_client(acc)
        try:
            client.rename_folder(old_name, new_name)
            return {"success": True}
        finally:
            client.logout()

    return await asyncio.to_thread(_rename)


@mcp.tool(
    description="Delete an IMAP folder. Refuses to delete INBOX or system folders. Requires account_id — call list_accounts to discover valid IDs (id or email address accepted)."
)
async def delete_folder(
    ctx: Context,
    account_id: str,
    name: str,
) -> dict:
    protected = {
        "INBOX",
        "[Gmail]",
        "[Gmail]/All Mail",
        "[Gmail]/Trash",
        "[Gmail]/Spam",
        "[Gmail]/Drafts",
        "[Gmail]/Sent Mail",
    }
    if name in protected:
        return {"error": f"Cannot delete protected folder: {name}"}

    accounts = ctx.lifespan_context["accounts"]
    acc = resolve_account(accounts, account_id)

    def _delete():
        client = get_imap_client(acc)
        try:
            client.delete_folder(name)
            return {"success": True}
        finally:
            client.logout()

    return await asyncio.to_thread(_delete)


@mcp.tool(
    description="List all configured email inboxes/accounts available on this server. Call this first to discover valid account_id values — every other email tool requires account_id. account_id also accepts the account's email address (from_address, imap_username, or smtp_username). This tool performs no IMAP connections and is safe to call frequently."
)
async def list_accounts(ctx: Context) -> list[dict]:
    accounts = ctx.lifespan_context["accounts"]
    return [
        {
            "id": acc["id"],
            "from_address": acc.get("from_address"),
            "imap_username": acc.get("imap_username"),
            "imap_host": acc.get("imap_host"),
            "smtp_host": acc.get("smtp_host"),
            "watch_folders": acc.get("watch_folders", []),
        }
        for acc in accounts
    ]


@mcp.tool(
    description="Get server name, version, webhook URL, and per-account connection status (performs a live IMAP check per account). For cheap inbox discovery without connecting, use list_accounts."
)
async def get_server_info(ctx: Context) -> dict:
    accounts = ctx.lifespan_context["accounts"]
    webhook_url = ctx.lifespan_context["webhook_url"]

    account_info = []
    for acc in accounts:
        status = "unknown"
        try:
            client = await asyncio.to_thread(get_imap_client, acc)
            await asyncio.to_thread(client.logout)
            status = "connected"
        except Exception as e:
            status = f"error: {e}"
        account_info.append(
            {
                "id": acc["id"],
                "from_address": acc["from_address"],
                "imap_host": acc["imap_host"],
                "smtp_host": acc["smtp_host"],
                "watch_folders": acc.get("watch_folders", []),
                "status": status,
            }
        )

    return {
        "server_name": "poke-mail",
        "version": "1.1.0",
        "accounts": account_info,
        "webhook_url": webhook_url,
    }


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    port = int(os.environ.get("PORT", 3000))
    host = "0.0.0.0"
    logger.info("Starting poke-mail on %s:%d", host, port)
    app = mcp.http_app(
        middleware=[Middleware(DropNonMCPRoutes), Middleware(RateLimitMiddleware)],
        stateless_http=True,
    )
    uvicorn.run(app, host=host, port=port)
