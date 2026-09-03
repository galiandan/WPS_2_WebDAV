"""HAR inspection and conservative first-pass redaction helpers.

This module never makes a network request. It is intended to make local
browser captures easier to inspect before a human shares a small excerpt.
"""

from __future__ import annotations

import copy
import json
import re
from typing import Any, Mapping
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit


REDACTED = "<redacted>"

_SENSITIVE_MARKERS = (
    "token",
    "access_token",
    "refresh_token",
    "id_token",
    "auth_token",
    "authorization",
    "cookie",
    "set_cookie",
    "password",
    "secret",
    "signature",
    "sig",
    "csrf",
    "credential",
    "session",
    "ticket",
    "upload_id",
)
_SENSITIVE_HEADERS = {
    "authorization",
    "cookie",
    "set-cookie",
    "proxy-authorization",
    "x-auth-token",
    "x-access-token",
    "x-refresh-token",
    "x-csrf-token",
}
_URL_HEADER_NAMES = {"referer", "location", "content-location"}


def _normalise_name(name: str) -> str:
    return re.sub(r"[^a-z0-9]+", "_", name.lower()).strip("_")


def is_sensitive_name(name: str) -> bool:
    normalised = _normalise_name(name)
    return any(marker in normalised for marker in _SENSITIVE_MARKERS)


def redact_url(value: str) -> str:
    """Keep URL shape and query names while removing credential-like values."""

    try:
        parts = urlsplit(value)
    except ValueError:
        return REDACTED

    netloc = parts.netloc
    if "@" in netloc:
        netloc = REDACTED + "@" + netloc.rsplit("@", 1)[1]

    query = []
    for key, query_value in parse_qsl(parts.query, keep_blank_values=True):
        query.append((key, REDACTED if is_sensitive_name(key) else query_value))

    fragment = REDACTED if parts.fragment else ""
    return urlunsplit((parts.scheme, netloc, parts.path, urlencode(query), fragment))


def _safe_path(path: str) -> str:
    """Keep route names while masking common numeric/hash object identifiers."""

    path = re.sub(r"(?<![A-Za-z0-9])\d{4,}(?![A-Za-z0-9])", "<id>", path)
    path = re.sub(r"(?<![A-Fa-f0-9])[A-Fa-f0-9]{16,}(?![A-Fa-f0-9])", "<id>", path)
    segments = []
    for segment in path.split("/"):
        if (
            len(segment) >= 10
            and re.fullmatch(r"[A-Za-z0-9_-]+", segment)
            and any(char.isupper() for char in segment)
            and any(char.islower() for char in segment)
            and any(char.isdigit() for char in segment)
        ):
            segment = "<id>"
        segments.append(segment)
    return "/".join(segments)


def safe_url_shape(value: str) -> str:
    """Return URL host/path and query names without any query values."""

    try:
        parts = urlsplit(value)
    except ValueError:
        return REDACTED

    netloc = parts.netloc
    if "@" in netloc:
        netloc = REDACTED + "@" + netloc.rsplit("@", 1)[1]
    query_names = [key for key, _ in parse_qsl(parts.query, keep_blank_values=True)]
    query = urlencode([(key, "<value>") for key in query_names])
    return urlunsplit((parts.scheme, netloc, _safe_path(parts.path), query, ""))


def _value_shape(value: Any, *, depth: int = 0) -> Any:
    """Describe JSON structure without returning JSON values."""

    if depth >= 6:
        return {"type": type(value).__name__, "truncated": True}
    if isinstance(value, Mapping):
        return {
            "type": "object",
            "keys": {
                str(key): _value_shape(item, depth=depth + 1)
                for key, item in value.items()
            },
        }
    if isinstance(value, list):
        result: dict[str, Any] = {"type": "array", "length": len(value)}
        if value:
            result["item"] = _value_shape(value[0], depth=depth + 1)
        return result
    if value is None:
        return {"type": "null"}
    if isinstance(value, bool):
        return {"type": "boolean"}
    if isinstance(value, (int, float)):
        return {"type": "number"}
    return {"type": "string"}


def _body_shape(data: Any) -> dict[str, Any]:
    if not isinstance(data, Mapping):
        return {"present": False}

    result: dict[str, Any] = {"present": True}
    params = data.get("params")
    if isinstance(params, list):
        fields = []
        for param in params:
            if isinstance(param, Mapping):
                fields.append({
                    "name": str(param.get("name", "")),
                    "has_file": "fileName" in param,
                })
        result["kind"] = "form-data"
        result["fields"] = fields
        return result

    text = data.get("text")
    mime_type = str(data.get("mimeType", "")).lower()
    if isinstance(text, str) and "json" in mime_type:
        try:
            result["kind"] = "json"
            result["shape"] = _value_shape(json.loads(text))
            return result
        except json.JSONDecodeError:
            pass
    if isinstance(text, str) and "x-www-form-urlencoded" in mime_type:
        result["kind"] = "urlencoded"
        result["fields"] = [key for key, _ in parse_qsl(text, keep_blank_values=True)]
        return result
    result["kind"] = "opaque"
    result["mime_type"] = str(data.get("mimeType", ""))
    return result


def _json_content_shape(content: Any) -> dict[str, Any]:
    if not isinstance(content, Mapping):
        return {"present": False}
    result: dict[str, Any] = {
        "present": True,
        "mime_type": str(content.get("mimeType", "")),
    }
    text = content.get("text")
    if isinstance(text, str) and "json" in result["mime_type"].lower():
        try:
            result["kind"] = "json"
            result["shape"] = _value_shape(json.loads(text))
            return result
        except json.JSONDecodeError:
            pass
    result["kind"] = "opaque"
    return result


def safe_entry_details(entry: Mapping[str, Any]) -> dict[str, Any]:
    """Return request/response structure with all values removed."""

    request = entry.get("request")
    response = entry.get("response")
    request = request if isinstance(request, Mapping) else {}
    response = response if isinstance(response, Mapping) else {}
    request_headers = request.get("headers")
    response_headers = response.get("headers")
    return {
        "request": {
            "method": str(request.get("method", "")),
            "url_shape": safe_url_shape(str(request.get("url", ""))),
            "header_names": [
                str(item.get("name", ""))
                for item in request_headers
                if isinstance(item, Mapping)
            ] if isinstance(request_headers, list) else [],
            "body": _body_shape(request.get("postData")),
        },
        "response": {
            "status": response.get("status"),
            "header_names": [
                str(item.get("name", ""))
                for item in response_headers
                if isinstance(item, Mapping)
            ] if isinstance(response_headers, list) else [],
            "content": _json_content_shape(response.get("content")),
        },
    }


def _redact_value(name: str, value: Any) -> Any:
    if is_sensitive_name(name):
        return REDACTED
    if isinstance(value, Mapping):
        return {key: _redact_value(str(key), item) for key, item in value.items()}
    if isinstance(value, list):
        return [_redact_value(name, item) for item in value]
    return value


def redact_headers(headers: list[dict[str, Any]]) -> list[dict[str, Any]]:
    result = []
    for header in headers:
        item = dict(header)
        name = str(item.get("name", ""))
        lower_name = name.lower()
        if lower_name in _SENSITIVE_HEADERS:
            item["value"] = REDACTED
        elif lower_name in _URL_HEADER_NAMES and isinstance(item.get("value"), str):
            item["value"] = redact_url(item["value"])
        result.append(item)
    return result


def _redact_post_data(post_data: Mapping[str, Any]) -> dict[str, Any]:
    item = dict(post_data)
    mime_type = str(item.get("mimeType", "")).lower()
    params = item.get("params")
    if isinstance(params, list):
        safe_params = []
        for param in params:
            if not isinstance(param, Mapping):
                safe_params.append(param)
                continue
            safe_param = dict(param)
            name = str(safe_param.get("name", ""))
            if is_sensitive_name(name):
                safe_param["value"] = REDACTED
            elif "fileName" in safe_param:
                # HAR multipart entries can contain a local file name and body.
                safe_param["fileName"] = REDACTED
                safe_param.pop("value", None)
            safe_params.append(safe_param)
        item["params"] = safe_params

    text = item.get("text")
    if isinstance(text, str):
        if "json" in mime_type:
            try:
                item["text"] = json.dumps(
                    _redact_value("body", json.loads(text)),
                    ensure_ascii=True,
                    separators=(",", ":"),
                )
            except json.JSONDecodeError:
                item["text"] = REDACTED
        elif "x-www-form-urlencoded" in mime_type:
            pairs = []
            for key, value in parse_qsl(text, keep_blank_values=True):
                pairs.append((key, REDACTED if is_sensitive_name(key) else value))
            item["text"] = urlencode(pairs)
        else:
            # This may be a multipart body, source file, or opaque binary text.
            item["text"] = REDACTED
    return item


def _redact_response_content(content: Mapping[str, Any]) -> dict[str, Any]:
    item = dict(content)
    mime_type = str(item.get("mimeType", "")).lower()
    text = item.get("text")
    if isinstance(text, str):
        if "json" in mime_type:
            try:
                item["text"] = json.dumps(
                    _redact_value("body", json.loads(text)),
                    ensure_ascii=True,
                    separators=(",", ":"),
                )
            except json.JSONDecodeError:
                item["text"] = REDACTED
        else:
            # Do not carry downloaded file contents into a shared HAR.
            item["text"] = REDACTED
            item.pop("encoding", None)
    return item


def redact_har(document: Mapping[str, Any]) -> dict[str, Any]:
    """Return a copy of a HAR with common credential and body locations masked."""

    result = copy.deepcopy(dict(document))
    log = result.get("log")
    if not isinstance(log, dict):
        return result

    pages = log.get("pages")
    if isinstance(pages, list):
        for page in pages:
            if isinstance(page, dict) and isinstance(page.get("title"), str):
                page["title"] = redact_url(page["title"])

    entries = log.get("entries")
    if not isinstance(entries, list):
        return result

    for entry in entries:
        if not isinstance(entry, dict):
            continue
        request = entry.get("request")
        if isinstance(request, dict):
            if isinstance(request.get("url"), str):
                request["url"] = redact_url(request["url"])
            if isinstance(request.get("headers"), list):
                request["headers"] = redact_headers(request["headers"])
            if isinstance(request.get("cookies"), list):
                for cookie in request["cookies"]:
                    if isinstance(cookie, dict):
                        cookie["value"] = REDACTED
            post_data = request.get("postData")
            if isinstance(post_data, Mapping):
                request["postData"] = _redact_post_data(post_data)

        response = entry.get("response")
        if isinstance(response, dict):
            if isinstance(response.get("headers"), list):
                response["headers"] = redact_headers(response["headers"])
            if isinstance(response.get("cookies"), list):
                for cookie in response["cookies"]:
                    if isinstance(cookie, dict):
                        cookie["value"] = REDACTED
            content = response.get("content")
            if isinstance(content, Mapping):
                response["content"] = _redact_response_content(content)
    return result


def _header_value(headers: Any, name: str) -> str:
    if not isinstance(headers, list):
        return ""
    for header in headers:
        if isinstance(header, Mapping) and str(header.get("name", "")).lower() == name:
            return str(header.get("value", ""))
    return ""


def summarize_entry(index: int, entry: Mapping[str, Any]) -> str:
    request = entry.get("request")
    response = entry.get("response")
    request = request if isinstance(request, Mapping) else {}
    response = response if isinstance(response, Mapping) else {}
    method = str(request.get("method", "?"))
    url = safe_url_shape(str(request.get("url", "")))
    status = response.get("status", "?")
    request_type = _header_value(request.get("headers"), "content-type")
    response_type = str(response.get("content", {}).get("mimeType", "")) if isinstance(response.get("content"), Mapping) else ""
    request_size = request.get("bodySize", "?")
    response_size = response.get("bodySize", "?")
    details = f"{index:03d} {method:<7} {status!s:<3} {url}"
    details += f" [request={request_size}, response={response_size}"
    if request_type:
        details += f", request_type={request_type}"
    if response_type:
        details += f", response_type={response_type}"
    details += "]"
    return details


def summarize_har(document: Mapping[str, Any]) -> list[str]:
    log = document.get("log")
    entries = log.get("entries") if isinstance(log, Mapping) else None
    if not isinstance(entries, list):
        return []
    return [
        summarize_entry(index, entry)
        for index, entry in enumerate(entries, start=1)
        if isinstance(entry, Mapping)
    ]
