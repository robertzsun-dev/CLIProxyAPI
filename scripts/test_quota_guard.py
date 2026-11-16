import contextlib
import copy
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

    def test_rejected_refresh_grant_is_not_replayed(self):
        for credential, headers in [
            ({'requires_sign_in': True}, {}),
            ({}, {'X-Quota-Auth': ['sign-in-required']}),
            ({}, {'x-quota-auth': ['sign-in-required']}),
        ]:
            with self.subTest(credential=credential, headers=headers):
                credential = {'auth_index': 'test', 'name': 'test.json', **credential}
                with patch.object(g, 'mgmt', return_value={'status_code': 401, 'header': headers}) as mgmt:
                    with self.assertRaisesRegex(RuntimeError, 'requires sign-in'):
                        g.upstream_get(credential, 'https://example.com', {})
                    self.assertEqual(mgmt.call_count, 1)

    def test_refreshable_access_token_is_refreshed_once(self):
        credential = {'auth_index': 'test', 'name': 'test.json'}
        with patch.object(g, 'mgmt', side_effect=[{'status_code': 401}, {}, {'status_code': 200, 'body': '{"quota": 1}'}]) as mgmt:
            self.assertEqual(g.upstream_get(credential, 'https://example.com', {}), {'quota': 1})
            self.assertEqual(mgmt.call_args_list[1].args, ('POST', '/credentials/refresh', {'name': 'test.json'}))
            self.assertEqual(mgmt.call_count, 3)

    def run_guard(self, now, *, disabled=False, guard_disabled=False, burn=False, session=40, weekly=80, failed=False, scoped=None, guard_excluded=None, recovery=False):
        cred = {'name': 'coworker.json', 'email': 'coworker@example.com', 'type': 'claude',
                'quota_owner': 'coworker', 'disabled': disabled, 'quota_burn': burn, 'priority': 101}
        if recovery:
            cred.update({'auth_index': 'test', 'cooldowns': [{'reason': 'quota', 'http_status': 429,
                         'model_key': 'claude-fable-5-2', 'retry_at': dt.datetime.fromtimestamp(now + 86400, dt.timezone.utc).isoformat()}],
                         'model_quotas': {'claude-fable-5-2': {'observed_at': dt.datetime.fromtimestamp(now - 3600, dt.timezone.utc).isoformat()}}})
        writes = []
        def mgmt(method, path, body=None):
            if method == 'GET':
                return {'files': [cred]}
            writes.append((path, body))
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp)/'state.json'
            state.write_text(json.dumps({'claude:coworker@example.com': {'guard_disabled': guard_disabled, 'excluded': guard_excluded or {}}}))
            with patch.object(g, 'STATE', str(state)), patch.object(g, 'STATUS', False), patch.object(g, 'DRY_RUN', False), patch.object(g.time, 'time', return_value=now), patch.object(g, 'mgmt', side_effect=mgmt), patch.object(g, 'claude_usage', side_effect=RuntimeError('unavailable') if failed else None, return_value=([weekly, now+86400], scoped or {}, [session, now+1000], now+1000 if recovery else None)), patch.object(g, 'set_guard_excludes', side_effect=lambda cred, old, new: writes.append(('exclusions', sorted(new)))), contextlib.redirect_stdout(io.StringIO()):
                g.main()
            return writes, json.loads(state.read_text())

    def test_claude_recovery_respects_reserves_and_failed_reads(self):
        for date, weekly, session, failed, expected in [
            ('2026-11-15T16:00:00+00:00', 80, 80, False, True),
            ('2026-11-15T16:00:00+00:00', 35, 80, False, False),
            ('2026-11-16T18:00:00+00:00', 80, 50, False, False),
            ('2026-11-15T16:00:00+00:00', 80, 80, True, False),
        ]:
            with self.subTest(date=date, weekly=weekly, session=session, failed=failed):
                now = at(date)
                writes, state = self.run_guard(now, weekly=weekly, session=session, failed=failed,
                                               scoped={'claude-fable-*': [100, now + 86400]}, recovery=True)
                reset = ('/routing/cooldown/reset', {'auth_index': 'test'})
                self.assertEqual(reset in writes, expected)
                if expected:
                    self.assertEqual(state['claude:coworker@example.com']['claude_recovery_at'], now)

    def test_fable_uses_remaining_allowance_and_releases_guard_exclusion(self):
        now = at('2026-11-15T16:00:00+00:00')
        reset = now + 86400
        for remaining in [35, 34, 1, 0]:
            with self.subTest(remaining=remaining):
                writes, state = self.run_guard(now, scoped={'claude-fable-*': [remaining, reset]},
                                               guard_excluded={'claude-fable-*': reset})
                expected = {'claude-fable-*': reset} if remaining == 0 else {}
                self.assertEqual(state['claude:coworker@example.com']['excluded'], expected)
                self.assertFalse(state['claude:coworker@example.com']['guard_disabled'])
                if remaining > 0:
                    self.assertIn(('exclusions', []), writes)

    def test_fable_does_not_bypass_shared_limits(self):
        for date, weekly, session in [
            ('2026-11-15T16:00:00+00:00', 35, 80),
            ('2026-11-15T16:00:00+00:00', 20, 80),
            ('2026-11-15T16:00:00+00:00', 0, 80),
            ('2026-11-16T18:00:00+00:00', 80, 50),
            ('2026-11-16T18:00:00+00:00', 80, 49),
        ]:
            with self.subTest(date=date, weekly=weekly, session=session):
                now = at(date)
                writes, state = self.run_guard(now, weekly=weekly, session=session,
                                               scoped={'claude-fable-*': [1, now + 86400]})
                self.assertIn(('/credentials/status', {'name': 'coworker.json', 'disabled': True}), writes)
                self.assertTrue(state['claude:coworker@example.com']['guard_disabled'])

    def test_other_model_family_keeps_weekly_reserve(self):
        now = at('2026-11-15T16:00:00+00:00')
        reset = now + 86400
        _, state = self.run_guard(now, scoped={'claude-sonnet-*': [10, reset]})
        self.assertEqual(state['claude:coworker@example.com']['excluded'], {'claude-sonnet-*': reset})

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


class ClaudeCooldownRecoveryTests(unittest.TestCase):
    def test_complete_quota_evidence_required(self):
        now = at('2026-11-16T04:00:00+00:00')
        reset = '2026-11-19T13:00:00+00:00'
        healthy = {'five_hour': None, 'limits': [
            {'kind': 'weekly_all', 'percent': 75, 'resets_at': reset},
            {'kind': 'weekly_scoped', 'percent': 0, 'resets_at': reset,
             'scope': {'model': {'display_name': 'Fable'}, 'surface': None}},
        ]}
        self.assertEqual(g.claude_recovery_until(healthy, now), at(reset))
        for index in [0, 1]:
            for used in [100, 101, -1, True, '0', None, float('nan')]:
                bad = copy.deepcopy(healthy)
                bad['limits'][index]['percent'] = used
                self.assertIsNone(g.claude_recovery_until(bad, now))
        bad_payloads = [
            {}, {**healthy, 'limits': []}, {**healthy, 'five_hour': {}},
            {**healthy, 'five_hour': {'utilization': 100, 'resets_at': reset}},
            {**healthy, 'seven_day': {'utilization': 100, 'resets_at': reset}},
            {**healthy, 'limits': healthy['limits'][1:]},
        ]
        for value in ['2026-11-15T00:00:00+00:00', 'bad-date', None]:
            bad = copy.deepcopy(healthy)
            bad['limits'][0]['resets_at'] = value
            bad_payloads.append(bad)
        for payload in bad_payloads:
            with self.subTest(payload=payload):
                self.assertIsNone(g.claude_recovery_until(payload, now))
        missing_session = copy.deepcopy(healthy)
        del missing_session['five_hour']
        self.assertIsNone(g.claude_recovery_until(missing_session, now))
        healthy['five_hour'] = {'utilization': 0, 'resets_at': None}
        self.assertEqual(g.claude_recovery_until(healthy, now), at(reset))

    def test_only_old_long_quota_holds_recover_once_per_interval(self):
        now = at('2026-11-16T04:00:00+00:00')
        cred = {'cooldowns': [{'reason': 'quota', 'http_status': 429, 'model_key': 'claude-fable-5-2',
                              'retry_at': '2026-11-19T13:00:00+00:00'}],
                'model_quotas': {'claude-fable-5-2': {'observed_at': '2026-11-15T04:00:00+00:00'}}}
        usage = {'at': now, 'claude_recovery_until': now + 86400,
                 'scoped': {'claude-fable-*': [100, now + 86400]}}
        state = {'usage': usage}
        self.assertTrue(g.claude_quota_recoverable(cred, state, now))
        for c, s in [
            ({**cred, 'disabled': True}, state),
            ({**cred, 'requires_sign_in': True}, state),
            ({**cred, 'cooldowns': []}, state),
            ({**cred, 'cooldowns': cred['cooldowns'] + [{'reason': 'unauthorized', 'http_status': 401}]}, state),
            ({**cred, 'model_quotas': {}}, state),
            ({**cred, 'model_quotas': {'claude-fable-5-2': {'observed_at': '2026-11-16T04:00:01+00:00'}}}, state),
            ({**cred, 'cooldowns': [{**cred['cooldowns'][0], 'retry_at': '2026-11-16T04:01:00+00:00'}]}, state),
            (cred, {'usage': {**usage, 'at': now - 121}}),
            (cred, {'usage': {**usage, 'at': now + 1}}),
            (cred, {'usage': {**usage, 'claude_recovery_until': None}}),
            (cred, {'usage': {**usage, 'claude_recovery_until': now}}),
            (cred, {'usage': {**usage, 'scoped': {}}}),
            (cred, {**state, 'claude_recovery_at': now - 30}),
        ]:
            with self.subTest(credential=c, state=s):
                self.assertFalse(g.claude_quota_recoverable(c, s, now))
        self.assertTrue(g.claude_quota_recoverable(cred, {**state, 'claude_recovery_at': now - 600}, now))


if __name__ == '__main__':
    unittest.main()
