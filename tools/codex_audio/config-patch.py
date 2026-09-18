"""只修改音频两项配置，保留原始文本；备份仅在用户私有运行目录。"""
import hashlib
import json
import pathlib
import re
import sys
import tomllib


def digest(data):
    return hashlib.sha256(data).hexdigest()


def patch(data, sideband):
    text = data.decode("utf-8-sig")
    parsed = tomllib.loads(text)
    if "experimental_realtime_ws_base_url" in parsed or "realtime_conversation" in parsed.get("features", {}):
        raise ValueError("已有音频设置，保留原值，需明确合并")
    if not re.fullmatch(r"ws://127\.0\.0\.1:\d{1,5}/v1", sideband):
        raise ValueError("Invalid local sideband URL")
    newline = "\r\n" if "\r\n" in text else "\n"
    top = f"# ConcordRoute Audio: 独立控制连接{newline}experimental_realtime_ws_base_url = {json.dumps(sideband)}{newline}"
    text = top + text
    header = re.search(r"(?m)^\[features\][ \t]*(?:#[^\r\n]*)?\r?$", text)
    feature = f"# ConcordRoute Audio: 仅启用 realtime 功能{newline}realtime_conversation = true{newline}"
    if header:
        end = text.find("\n", header.end())
        if end == -1:
            text += newline + feature
        else:
            text = text[:end + 1] + feature + text[end + 1:]
    else:
        text += newline + "[features]" + newline + feature
    result = text.encode("utf-8")
    check = tomllib.loads(text)
    assert check["experimental_realtime_ws_base_url"] == sideband
    assert check["features"]["realtime_conversation"] is True
    return result


def main():
    mode, target, directory, *args = sys.argv[1:]
    target, directory = pathlib.Path(target), pathlib.Path(directory)
    backup, manifest = directory / "config.before.toml", directory / "config-change.json"
    current = target.read_bytes()
    if mode == "apply":
        if manifest.exists():
            record = json.loads(manifest.read_text())
            if digest(current) == record["after"]:
                print("CONFIG_ALREADY_INSTALLED")
                return
            raise ValueError("配置已变更，保留现场")
        modified = patch(current, args[0])
        backup.write_bytes(current)
        manifest.write_text(json.dumps({"before": digest(current), "after": digest(modified)}), encoding="utf-8")
        target.write_bytes(modified)
        print("CONFIG_AUDIO_ONLY_APPLIED")
    elif mode == "restore":
        record = json.loads(manifest.read_text())
        if digest(current) == record["before"]:
            print("CONFIG_ALREADY_RESTORED")
            return
        if digest(current) != record["after"]:
            raise ValueError("配置存在后续编辑，停止覆盖")
        original = backup.read_bytes()
        assert digest(original) == record["before"]
        target.write_bytes(original)
        print("CONFIG_EXACTLY_RESTORED")
    else:
        raise ValueError("Unknown mode")


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("CONFIG_OPERATION_STOPPED", file=sys.stderr)
        sys.exit(1)
