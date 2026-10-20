#!/usr/bin/env python3
"""Check the stdio server; an explicit --query also performs one live OPS search."""

import argparse
import json
import os
from pathlib import Path
import queue
import subprocess
import sys
import threading
import time


class SmokeFailure(Exception):
    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--query", help="Explicit CQL query for one live OPS call; omitted by default.")
    arguments = parser.parse_args()
    if not os.environ.get("EPO_CLIENT_KEY") or not os.environ.get("EPO_CLIENT_SECRET"):
        raise SmokeFailure("Set EPO_CLIENT_KEY and EPO_CLIENT_SECRET in the environment first.")

    repo = Path(__file__).resolve().parent
    process = subprocess.Popen([str(repo / "launch.sh")], cwd=repo, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
                               encoding="utf-8", bufsize=1)
    responses = queue.Queue()

    def read_responses():
        try:
            for line in process.stdout:
                try:
                    responses.put(json.loads(line))
                except (ValueError, UnicodeError):
                    responses.put(SmokeFailure("The MCP server emitted invalid JSON."))
                    return
        finally:
            responses.put(None)

    threading.Thread(target=read_responses, daemon=True).start()

    def send(message):
        try:
            process.stdin.write(json.dumps(message) + "\n")
            process.stdin.flush()
        except (BrokenPipeError, OSError):
            raise SmokeFailure("The MCP server stopped accepting requests.") from None

    def request(identifier, method, params=None, timeout=10):
        message = {"jsonrpc": "2.0", "id": identifier, "method": method}
        if params is not None:
            message["params"] = params
        send(message)
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise SmokeFailure(f"The MCP {method} request timed out.")
            try:
                response = responses.get(timeout=remaining)
            except queue.Empty:
                raise SmokeFailure(f"The MCP {method} request timed out.") from None
            if response is None:
                raise SmokeFailure(f"The MCP server exited before answering {method}.")
            if isinstance(response, SmokeFailure):
                raise response
            if not isinstance(response, dict):
                raise SmokeFailure("The MCP response is not a JSON object.")
            if response.get("id") == identifier:
                if response.get("jsonrpc") != "2.0":
                    raise SmokeFailure("The MCP response has an unexpected JSON-RPC version.")
                return response

    try:
        response = request(1, "initialize", {
            "protocolVersion": "2024-11-05", "capabilities": {},
            "clientInfo": {"name": "epo-ops-mcp-smoke", "version": "1"},
        })
        if response.get("error") or response.get("result", {}).get("protocolVersion") != "2024-11-05":
            raise SmokeFailure("MCP initialization failed.")
        send({"jsonrpc": "2.0", "method": "notifications/initialized"})

        response = request(2, "tools/list")
        tools = response.get("result", {}).get("tools", [])
        expected = {"search_patents", "get_patent_abstracts", "get_patent_fulltext", "get_usage_stats"}
        if response.get("error") or len(tools) != 4 or {tool.get("name") for tool in tools} != expected:
            raise SmokeFailure("MCP tool discovery did not return the four expected tools.")

        response = request(3, "tools/call", {"name": "search_patents", "arguments": {}})
        if response.get("error") or response.get("result", {}).get("isError") is not True:
            raise SmokeFailure("The query tool did not reject missing required arguments.")
        print("PASS: initialize; tools/list returned four tools; invalid input rejected.")

        if arguments.query is not None:
            response = request(4, "tools/call", {
                "name": "search_patents", "arguments": {"keywords": arguments.query, "max_results": 1},
            }, timeout=70)
            result = response.get("result", {})
            if response.get("error") or result.get("isError"):
                raise SmokeFailure("The live OPS query failed; provider details are suppressed.")
            try:
                content = result["content"]
                payload = json.loads(next(item["text"] for item in content if item.get("type") == "text"))
                total = payload["total_results"]
                records = payload["results"]
                if not isinstance(total, int) or not isinstance(records, list) or payload.get("status") == "failed":
                    raise ValueError
            except (KeyError, TypeError, ValueError, StopIteration):
                raise SmokeFailure("The live OPS response did not contain a valid search result.") from None
            print(f"PASS: live OPS query; total_results={total}; returned={len(records)}.")

        process.stdin.close()
        try:
            exit_code = process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            raise SmokeFailure("The MCP server did not stop after input EOF.") from None
        if exit_code != 0:
            raise SmokeFailure("The MCP server exited unsuccessfully after input EOF.")
        print("PASS: input EOF stopped the MCP server with exit code 0.")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)
        process.stdout.close()
        if not process.stdin.closed:
            process.stdin.close()


if __name__ == "__main__":
    try:
        main()
    except SmokeFailure as error:
        print(f"Smoke test failed: {error}", file=sys.stderr)
        sys.exit(1)
    except OSError:
        print("Smoke test failed: could not launch or communicate with the MCP process.", file=sys.stderr)
        sys.exit(1)
