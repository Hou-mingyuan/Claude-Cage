#!/usr/bin/env python3
"""Scoped MCP server for reading workspace status and adjusting safe workbench settings."""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from typing import Any

TOOLS: list[dict[str, Any]] = [
    {
        "name": "workspace_list_capabilities",
        "description": "List which appearance settings, display preferences, and extension actions can be inspected or adjusted.",
        "inputSchema": {"type": "object", "properties": {}},
        "_meta": {"ui": {"visibility": ["model"]}},
    },
    {
        "name": "workspace_get_status",
        "description": "Read current appearance settings, display preferences, open project folders, extension summaries, and pending draft count.",
        "inputSchema": {"type": "object", "properties": {}},
        "_meta": {"ui": {"visibility": ["model"]}},
    },
    {
        "name": "workspace_update_appearance",
        "description": "Update workbench appearance settings such as light/dark mode, UI/code font sizes, motion preference, and accent or surface hex colors.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "mode": {"type": "string", "enum": ["system", "light", "dark"], "description": "Color scheme mode."},
                "ui_font_size": {"type": "integer", "minimum": 12, "maximum": 24, "description": "Interface font size in pixels (12-24)."},
                "code_font_size": {"type": "integer", "minimum": 10, "maximum": 24, "description": "Code and terminal font size in pixels (10-24)."},
                "pointer_cursors": {"type": "boolean", "description": "Use pointer cursors on interactive controls."},
                "font_smoothing": {"type": "boolean", "description": "Enable antialiased font smoothing."},
                "reduced_motion": {"type": "string", "enum": ["system", "on", "off"], "description": "Animation reduction preference."},
                "light_accent": {"type": "string", "description": "Six-digit hex accent color for light theme (e.g. #2563eb)."},
                "dark_accent": {"type": "string", "description": "Six-digit hex accent color for dark theme (e.g. #60a5fa)."},
                "light_surface": {"type": "string", "description": "Six-digit hex surface color for light theme."},
                "dark_surface": {"type": "string", "description": "Six-digit hex surface color for dark theme."},
                "light_ink": {"type": "string", "description": "Six-digit hex text color for light theme."},
                "dark_ink": {"type": "string", "description": "Six-digit hex text color for dark theme."},
            },
        },
        "_meta": {"ui": {"visibility": ["model"]}},
    },
    {
        "name": "workspace_update_preferences",
        "description": "Update safe display and terminal preferences such as balanced display, clock format, automatic timezone/language, project file cache, browser home URL, or terminal command.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "balanced_display": {"type": "boolean", "description": "Use balanced interface density."},
                "clock_12_hour": {"type": "boolean", "description": "Display 12-hour clock format instead of 24-hour."},
                "auto_timezone": {"type": "boolean", "description": "Automatically sync timezone."},
                "auto_language": {"type": "boolean", "description": "Automatically follow system language."},
                "project_file_cache": {"type": "boolean", "description": "Enable workspace project file caching."},
                "browser_home_url": {"type": "string", "description": "Default homepage URL for the built-in browser (http or https)."},
                "claude_terminal_command": {"type": "string", "description": "Default CLI launch command in new terminals."},
            },
        },
        "_meta": {"ui": {"visibility": ["model"]}},
    },
    {
        "name": "workspace_manage_extension",
        "description": "Enable or disable a non-core extension, or trigger an extension reload. Always requires explicit user confirmation.",
        "inputSchema": {
            "type": "object",
            "required": ["action"],
            "properties": {
                "action": {"type": "string", "enum": ["enable", "disable", "reload"], "description": "Extension lifecycle action to perform."},
                "name": {"type": "string", "description": "Extension identifier (required for enable and disable)."},
            },
        },
        "_meta": {"ui": {"visibility": ["model"]}},
    },
]

CAPABILITIES_INFO = {
    "appearance": {
        "mode": ["system", "light", "dark"],
        "ui_font_size": "12..24",
        "code_font_size": "10..24",
        "pointer_cursors": "boolean",
        "font_smoothing": "boolean",
        "reduced_motion": ["system", "on", "off"],
        "colors": ["light_accent", "dark_accent", "light_surface", "dark_surface", "light_ink", "dark_ink (#RRGGBB)"],
    },
    "preferences": {
        "balanced_display": "boolean",
        "clock_12_hour": "boolean",
        "auto_timezone": "boolean",
        "auto_language": "boolean",
        "project_file_cache": "boolean",
        "browser_home_url": "http(s):// URL",
        "claude_terminal_command": "non-empty command string",
    },
    "extensions": {
        "actions": ["enable", "disable", "reload"],
        "confirmation": "always asked per call",
    },
    "manual_only_boundaries": [
        "Adding or removing shared project folders",
        "Modifying network or proxy routing rules",
        "Granting or revoking always-allow permissions",
        "Account credentials, passwords, and encrypted secrets",
        "Uninstalling extensions or modifying built-in core tools",
    ],
}


def api_call(method: str, path: str, payload: dict[str, Any] | None = None) -> tuple[bool, Any]:
    base_url = os.environ.get("WORKSPACE_CONTROL_URL", "").rstrip("/")
    token = os.environ.get("WORKSPACE_CONTROL_TOKEN", "").strip()
    if not base_url or not token:
        return False, "control bridge is not configured in this session"

    data = None
    headers = {"Authorization": f"Bearer {token}", "Accept": "application/json"}
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"

    req = urllib.request.Request(f"{base_url}{path}", data=data, headers=headers, method=method)
    # Bypass system HTTP proxies for local loopback calls.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=15) as resp:
            raw = resp.read().decode("utf-8")
            return True, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace").strip()
        return False, body or f"request failed with status {exc.code}"
    except Exception as exc:
        return False, f"control bridge request failed: {exc}"


def ok_json(payload: Any) -> dict[str, Any]:
    return {"content": [{"type": "text", "text": json.dumps(payload, indent=2, ensure_ascii=False)}]}


def err_result(message: str) -> dict[str, Any]:
    return {"isError": True, "content": [{"type": "text", "text": message}]}


def handle_tool_call(name: str, args: dict[str, Any]) -> dict[str, Any]:
    args = args or {}
    if name == "workspace_list_capabilities":
        return ok_json(CAPABILITIES_INFO)
    if name == "workspace_get_status":
        ok, res = api_call("GET", "/status")
        return ok_json(res) if ok else err_result(str(res))
    if name == "workspace_update_appearance":
        if not args:
            return err_result("provide at least one appearance field to update")
        ok, res = api_call("POST", "/appearance", args)
        return ok_json(res) if ok else err_result(str(res))
    if name == "workspace_update_preferences":
        if not args:
            return err_result("provide at least one preference field to update")
        ok, res = api_call("POST", "/preferences", args)
        return ok_json(res) if ok else err_result(str(res))
    if name == "workspace_manage_extension":
        action = args.get("action", "")
        if action not in ("enable", "disable", "reload"):
            return err_result("action must be enable, disable, or reload")
        if action in ("enable", "disable") and not args.get("name"):
            return err_result("name is required for enable and disable")
        ok, res = api_call("POST", "/extensions", {"action": action, "name": args.get("name", "")})
        return ok_json(res) if ok else err_result(str(res))
    return err_result(f"unknown tool: {name}")


def answer(message: dict[str, Any]) -> dict[str, Any] | None:
    method = message.get("method")
    params = message.get("params") or {}
    if method == "initialize":
        return {
            "protocolVersion": params.get("protocolVersion", "2024-11-05"),
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "workspace-control", "version": "0.1.0"},
        }
    if method == "tools/list":
        return {"tools": TOOLS}
    if method == "tools/call":
        return handle_tool_call(params.get("name", ""), params.get("arguments") or {})
    if method == "ping":
        return {}
    return None


def main() -> None:
    for raw in sys.stdin:
        line = raw.strip()
        if not line:
            continue
        request = json.loads(line)
        if "id" not in request:
            continue
        result = answer(request)
        reply: dict[str, Any] = {"jsonrpc": "2.0", "id": request["id"]}
        if result is None:
            reply["error"] = {"code": -32601, "message": "method not found"}
        else:
            reply["result"] = result
        sys.stdout.write(json.dumps(reply) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
