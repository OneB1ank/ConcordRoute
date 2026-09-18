"""音频配置仅增量修改，检查原 provider、UA 和其它设置以及逐字节回滚。"""
import importlib.util
import pathlib
import subprocess
import sys
import tempfile
import tomllib
import unittest

MODULE = pathlib.Path(__file__).with_name("config-patch.py")
spec = importlib.util.spec_from_file_location("audio_config", MODULE)
config = importlib.util.module_from_spec(spec)
spec.loader.exec_module(config)


class AudioConfigTests(unittest.TestCase):
    def test_preserve_existing_features_and_provider(self):
        original = b'model_provider="custom"\r\n[features]\r\nother=true\r\n[model_providers.custom]\r\nbase_url="http://127.0.0.1:8080"\r\n'
        patched = tomllib.loads(config.patch(original, "ws://127.0.0.1:19847/v1").decode())
        self.assertTrue(patched["features"].pop("realtime_conversation"))
        self.assertEqual(patched.pop("experimental_realtime_ws_base_url"), "ws://127.0.0.1:19847/v1")
        self.assertEqual(patched, tomllib.loads(original.decode()))

    def test_refuses_existing_audio_configuration(self):
        for source in [b'experimental_realtime_ws_base_url="wss://existing.invalid"\n',
                       b"[features]\nrealtime_conversation=false\n"]:
            with self.assertRaises(ValueError):
                config.patch(source, "ws://127.0.0.1:19847/v1")

    def test_exact_restore_and_preserve_later_edits(self):
        with tempfile.TemporaryDirectory(prefix="audio-config-test-") as folder:
            target = pathlib.Path(folder) / "config.toml"
            original = b'# original comment\nmodel="fixture"\n[model_providers.fixture]\nname="fixture"\n'
            target.write_bytes(original)
            def run(mode, *args):
                return subprocess.run([sys.executable, str(MODULE), mode, str(target), folder, *args],
                                      capture_output=True, text=True)
            self.assertEqual(run("apply", "ws://127.0.0.1:19847/v1").returncode, 0)
            modified = target.read_bytes()
            target.write_bytes(modified + b"# later user edit\n")
            self.assertEqual(run("restore").returncode, 1)
            self.assertIn(b"later user edit", target.read_bytes())
            target.write_bytes(modified)
            self.assertEqual(run("restore").returncode, 0)
            self.assertEqual(target.read_bytes(), original)


if __name__ == "__main__":
    unittest.main()
