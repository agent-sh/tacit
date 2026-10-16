"""Synthetic portability and offline isolation guards. No live configuration."""
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import harness


class OfflineGuards(unittest.TestCase):
    def test_custom_audit_root_and_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            self.assertEqual(harness.private_output(root/'explicit', root), root/'explicit')
            with patch.dict(os.environ, {'TACIT_SELECTION_AUDIT_DIR': str(root)}):
                self.assertEqual(harness.private_output(root/'environment'), root/'environment')
            with self.assertRaises(ValueError):
                harness.private_output(root.parent/'outside-audit', root)

    def test_repository_output_rejected_even_with_custom_root(self):
        repo = Path(harness.__file__).resolve().parents[2]
        with self.assertRaisesRegex(ValueError, 'outside the repository'):
            harness.private_output(repo/'must-not-create', repo)
        with self.assertRaisesRegex(ValueError, 'outside the repository'):
            harness.private_output(repo/'must-not-create', repo.parent)

    def test_directory_symlink_cannot_escape_audit_root(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            audit = root/'audit'
            audit.mkdir()
            (audit/'escape').symlink_to(root, target_is_directory=True)
            with self.assertRaises(ValueError):
                harness.private_output(audit/'escape'/'must-not-create', audit)
            self.assertFalse((root/'must-not-create').exists())

    def test_retrieval_environment_ignores_live_utility_and_config(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory).resolve()
            inherited = {'TACIT_UTILITY_PROVIDER': 'jev', 'TACIT_SETTINGS': 'do-not-read.yaml',
                         'TACIT_UTILITY_KEY_FILE': 'do-not-read.key',
                         'TYPESAFE_API_KEY': 'synthetic-not-a-key', 'TACIT_TEST_MAIN': '1',
                         'TACIT_MODEL_JOB': '1', 'TACIT_LOCAL': 'do-not-read-overlay'}
            with patch.dict(os.environ, inherited):
                env = harness.retrieval_env(out, out/'synthetic-store')
            self.assertEqual(env['TACIT_UTILITY_PROVIDER'], 'disabled')
            for key in ('TYPESAFE_API_KEY', 'TACIT_UTILITY_KEY_FILE', 'TACIT_TEST_MAIN', 'TACIT_MODEL_JOB'):
                self.assertNotIn(key, env)
            self.assertEqual(Path(env['TACIT_SETTINGS']).read_text(), '{}\n')
            self.assertEqual(Path(env['HOME']), out/'isolated-home')
            self.assertEqual(Path(env['TACIT_LOCAL']), out/'empty-overlay')
            self.assertEqual(env['GIT_CONFIG_GLOBAL'], os.devnull)


if __name__ == '__main__':
    unittest.main()
