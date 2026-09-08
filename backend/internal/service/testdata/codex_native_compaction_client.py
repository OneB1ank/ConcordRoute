"""隔离运行真实客户端；只使用合成会话、回环上游与空白配置。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import queue
import subprocess
import threading
import time
from urllib.parse import urlparse


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--url", required=True)
    parser.add_argument("--mode", choices=["summary", "legacy", "v2"], required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--auto", action="store_true")
    parser.add_argument("--history-repetitions", type=int, default=512)
    args = parser.parse_args()
    assert urlparse(args.url).hostname == "127.0.0.1", "测试仅允许回环地址"
    out = Path(args.out).resolve()
    home = out / "home"
    work = out / "work"
    home.mkdir(parents=True, exist_ok=True)
    work.mkdir(exist_ok=True)
    # 私有 HOME 与工作目录不继承用户的授权、技能、项目或长会话。
    config = f'''
model = "gpt-6-astra"
model_provider = "synthetic"
approval_policy = "never"
sandbox_mode = "read-only"
model_auto_compact_token_limit = {200000 if args.auto else 1000000}
compact_prompt = "SYNTHETIC_SUMMARIZE_ONLY"
web_search = "disabled"
check_for_update_on_startup = false
[analytics]
enabled = false
[feedback]
enabled = false
[features]
remote_compaction_v2 = {str(args.mode == "v2").lower()}
shell_snapshot = false
memory_tool = false
personality = false
[model_providers.synthetic]
name = "{'custom' if args.mode == 'summary' else 'OpenAI'}"
base_url = "{args.url}"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
'''
    (home / "config.toml").write_text(config, encoding="utf-8")
    env = {
        k: v for k, v in os.environ.items()
        if not any(term in k.upper() for term in
                   ["TOKEN", "AUTH", "API_KEY", "OPENAI", "ANTHROPIC", "CODEX", "PROXY"])
    }
    env.update({
        "CODEX_HOME": str(home), "HOME": str(home), "USERPROFILE": str(home),
        "HTTP_PROXY": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1",
        "ALL_PROXY": "http://127.0.0.1:1", "NO_PROXY": "127.0.0.1,localhost",
    })
    messages = queue.Queue()
    all_messages = []
    stderr = (out / "client.stderr.log").open("w", encoding="utf-8")
    process = subprocess.Popen(
        [args.binary, "app-server", "--listen", "stdio://"], cwd=work, env=env,
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr,
        text=True, encoding="utf-8", creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )

    def reader():
        for line in process.stdout:
            try:
                item = json.loads(line)
                all_messages.append(item)
                messages.put(item)
            except json.JSONDecodeError:
                messages.put({"parse_error": line})
        messages.put({"process_eof": True})

    reader_thread = threading.Thread(target=reader, daemon=True)
    reader_thread.start()
    request_id = 0

    def send(method, params=None, request=True):
        nonlocal request_id
        message = {"method": method}
        if params is not None:
            message["params"] = params
        if request:
            request_id += 1
            message["id"] = request_id
        process.stdin.write(json.dumps(message, ensure_ascii=False) + "\n")
        process.stdin.flush()
        return request_id

    def wait(predicate, seconds=30):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            item = messages.get(timeout=max(0.05, deadline - time.monotonic()))
            if "process_eof" in item:
                raise RuntimeError("客户端提前退出")
            if item.get("method") == "error" or "error" in item:
                raise RuntimeError(json.dumps(item, ensure_ascii=False))
            if predicate(item):
                return item
        raise TimeoutError("等待客户端事件超时")

    def rpc(method, params):
        request = send(method, params)
        return wait(lambda item: item.get("id") == request)["result"]

    def turn(thread, text):
        result = rpc("turn/start", {
            "threadId": thread, "input": [{"type": "text", "text": text, "text_elements": []}],
        })
        tid = result["turn"]["id"]
        completed = wait(lambda item: item.get("method") == "turn/completed"
                         and item["params"]["turn"]["id"] == tid)
        assert completed["params"]["turn"]["status"] == "completed", completed

    try:
        init = rpc("initialize", {
            "clientInfo": {"name": "synthetic_compact_test", "version": "1.0"},
            "capabilities": {"experimentalApi": True},
        })
        send("initialized", request=False)
        thread = rpc("thread/start", {
            "cwd": str(work), "model": "gpt-6-astra", "approvalPolicy": "never",
            "sandbox": "read-only",
        })["thread"]["id"]
        turn(thread, "SYNTHETIC_USER_FIRST " + "stable prefix " * args.history_repetitions)
        turn(thread, "SYNTHETIC_USER_SECOND")
        if not args.auto:
            rpc("thread/compact/start", {"threadId": thread})
            compact_done = wait(lambda item: item.get("method") == "item/completed"
                                and item["params"]["item"].get("type") == "contextCompaction")
            # 条目结束先于回合释放；等待任务真正结束，避免抢跑而触发 ActiveTurnNotSteerable。
            wait(lambda item: item.get("method") == "turn/completed"
                 and item["params"]["turn"]["id"] == compact_done["params"]["turnId"])
        turn(thread, "SYNTHETIC_AFTER_COMPACT")
        turn(thread, "SYNTHETIC_WARM_FOLLOWUP")
        compact_events = [
            e for e in all_messages if e.get("method") == "item/completed"
            and e["params"]["item"].get("type") == "contextCompaction"
        ]
        assert len(compact_events) == 1, f"压缩完成事件数量异常：{len(compact_events)}"
        with open(args.binary, "rb") as binary_file:
            binary_hash = hashlib.file_digest(binary_file, "sha256").hexdigest()
        result = {
            "mode": args.mode, "status": "PASS",
            "automatic": args.auto, "history_repetitions": args.history_repetitions,
            "client": init, "thread": thread, "real_upstream": False,
            "binary_sha256": binary_hash,
        }
        (out / "client-result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
        print(json.dumps(result, ensure_ascii=False), flush=True)
    finally:
        process.stdin.close()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
        reader_thread.join(timeout=5)
        process.stdout.close()
        stderr.close()
        (out / "client-events.json").write_text(
            json.dumps(all_messages, ensure_ascii=False, indent=2), encoding="utf-8"
        )


if __name__ == "__main__":
    main()
