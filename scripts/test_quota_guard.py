import contextlib
import datetime as dt
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('quota_guard', Path(__file__).with_name('quota-guard.py'))
g = importlib.util.module_from_spec(spec)
spec.loader.exec_module(g)


def at(value):
    return dt.datetime.fromisoformat(value).timestamp()


class SessionReserveTests(unittest.TestCase):
    def test_work_hours_and_dst(self):
        for value, expected in [
            ('2026-10-26T14:59:59+00:00', False),
            ('2026-10-26T15:00:00+00:00', True),
            ('2026-10-27T00:59:59+00:00', True),
            ('2026-10-27T01:00:00+00:00', False),
            ('2026-11-02T15:59:59+00:00', False),
            ('2026-11-02T16:00:00+00:00', True),
            ('2026-10-25T18:00:00+00:00', False),
        ]:
            with self.subTest(value=value):
                self.assertEqual(g.work_hours(at(value)), expected)

    def test_usage_formats_and_invalid_values(self):
        reset = '2026-10-26T22:00:00+00:00'
        for payload in [
            {'five_hour': {'utilization': 50, 'resets_at': reset}},
            {'limits': [{'kind': 'session', 'percent': 50, 'resets_at': reset}]},
        ]:
            self.assertEqual(g.claude_session_window(payload), [50, at(reset)])
        self.assertEqual(g.claude_session_window({'five_hour': {'utilization': 0, 'resets_at': None}}), [100, None])
        for used in [None, True, -1, 101, '50', float('nan')]:
            self.assertIsNone(g.claude_session_window({'five_hour': {'utilization': used, 'resets_at': reset}}))
        self.assertIsNone(g.claude_session_window({}))
        self.assertIsNone(g.claude_session_window({'five_hour': {'utilization': 10, 'resets_at': None}}))

    def test_floor_and_unknown_usage(self):
        now = at('2026-10-26T18:00:00+00:00')
        for remaining, held in [(51, False), (50, True), (49, True), (0, True)]:
            usage = {'session': [remaining, now + 1000], 'at': now}
            self.assertEqual(g.session_hold('claude', False, usage, now) is not None, held)
            self.assertIsNone(g.session_hold('claude', True, usage, now))
            self.assertIsNone(g.session_hold('codex', False, usage, now))
        for usage in [None, {'at': now}, {'at': now - 121, 'session': [100, None]}, {'at': now, 'session': [100, now]}]:
            self.assertIsNotNone(g.session_hold('claude', False, usage, now))
        self.assertIsNone(g.session_hold('claude', False, None, at('2026-10-27T01:00:00+00:00')))

    def test_stale_proxy_response_not_treated_as_fresh(self):
        with patch.object(g, 'mgmt', return_value={'status_code': 200, 'header': {'X-Quota-Cache': ['stale']}, 'body': '{}'}):
            with self.assertRaisesRegex(RuntimeError, 'stale'):
                g.upstream_get({'auth_index': 'test'}, 'https://example.com', {})

    def run_guard(self, now, *, disabled=False, guard_disabled=False, burn=False, session=40, weekly=80, failed=False):
        cred = {'name': 'coworker.json', 'email': 'coworker@example.com', 'type': 'claude',
                'quota_owner': 'coworker', 'disabled': disabled, 'quota_burn': burn, 'priority': 101}
        writes = []
        def mgmt(method, path, body=None):
            if method == 'GET':
                return {'files': [cred]}
            writes.append((path, body))
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp)/'state.json'
            state.write_text(json.dumps({'claude:coworker@example.com': {'guard_disabled': guard_disabled}}))
            with patch.object(g, 'STATE', str(state)), patch.object(g, 'STATUS', False), patch.object(g, 'DRY_RUN', False), patch.object(g.time, 'time', return_value=now), patch.object(g, 'mgmt', side_effect=mgmt), patch.object(g, 'claude_usage', side_effect=RuntimeError('unavailable') if failed else None, return_value=([weekly, now+86400], {}, [session, now+1000])), contextlib.redirect_stdout(io.StringIO()):
                g.main()
            return writes, json.loads(state.read_text())

    def test_burn_does_not_override_reserve(self):
        writes, state = self.run_guard(at('2026-10-26T18:00:00+00:00'), burn=True)
        self.assertIn(('/credentials/status', {'name': 'coworker.json', 'disabled': True}), writes)
        self.assertTrue(state['claude:coworker@example.com']['guard_disabled'])

    def test_recovery_and_evening_release(self):
        for now, remaining in [('2026-10-26T18:00:00+00:00', 80), ('2026-10-27T01:00:00+00:00', 40)]:
            writes, _ = self.run_guard(at(now), disabled=True, guard_disabled=True, session=remaining)
            self.assertIn(('/credentials/status', {'name': 'coworker.json', 'disabled': False}), writes)

    def test_weekly_floor_still_applies_after_work(self):
        writes, _ = self.run_guard(at('2026-10-27T01:00:00+00:00'), weekly=20)
        self.assertIn(('/credentials/status', {'name': 'coworker.json', 'disabled': True}), writes)

    def test_missing_usage_holds_account_and_manual_disable_survives(self):
        now = at('2026-10-26T18:00:00+00:00')
        writes, _ = self.run_guard(now, failed=True)
        self.assertIn(('/credentials/status', {'name': 'coworker.json', 'disabled': True}), writes)
        writes, _ = self.run_guard(now, disabled=True, session=100)
        self.assertEqual(writes, [])


if __name__ == '__main__':
    unittest.main()
