#!/usr/bin/python3
"""Order CLIProxyAPI's Claude/Codex accounts by weekly reset, and hold the
accounts that aren't Robert's to quota floors.

Coworker Claude accounts retain 50% of their five-hour session allowance on
weekdays from 08:00 to 18:00 America/Los_Angeles (including DST). This reserve
also applies to burn/final tiers and the Burn switch. Missing or stale session
usage holds the credential until fresh usage confirms it is above the floor.
This polling guard stops new routed requests; it cannot reclaim usage consumed
by requests already in flight or by the account owner outside this proxy.

CLIProxyAPI only routes to the highest-priority available accounts, so the
guard rewrites priorities every run (larger is used first):

  final tier  any account whose weekly limit resets within FINAL_WINDOW
  burn tier   any account whose weekly limit resets within BURN_WINDOW
              (in both, the remaining quota is about to be lost anyway, so
              whose account it is does not matter)
  burning     accounts with the panel's "Burn" switch on (see below)
  own tier    Robert's accounts ("personal" owner, see below)
  guest tier  everyone else's accounts

Inside a tier, the account that resets soonest ranks highest. Robert's accounts
have no floor; once one is exhausted it drops to priority 1 so it stops being
tried first.

Codex credits are the last resort for every account (Robert's choice,
2026-10-04). Once any Codex limit is reached (the 5-hour window as well as the
weekly one), OpenAI keeps serving the account and bills its credits. Such an
account leaves its tier for the credits tier, below all remaining plan quota:
Robert's accounts at CREDITS_PRIORITY["own"], everyone else's just under them,
largest balance first. A guest that is disabled by its floor is enabled again
only while its plan limit is reached, so its floor quota itself stays untouched.
Accounts near or at a limit are re-read every run, like guests. Other accounts are disabled once their weekly limit has FLOOR
percent or less left, BURN_FLOOR within BURN_WINDOW of the reset, or 0% within
FINAL_WINDOW (the remainder is lost at the reset anyway), and are
re-enabled when the window resets or when the burn window opens with quota above
BURN_FLOOR. Claude's model-scoped weekly limits (e.g. Fable) get the same floors
by excluding just that model family on the account.

Stale Codex "quota" cooldowns are cleared once fresh usage shows the account has
quota again (the proxy keeps the 429's reset time even after an early reset).

Claude 404 cooldowns are cleared every run: the proxy benches a model on an
account for 12 hours after any 404, but on Claude subscriptions a 404 is a
request-specific "No thread state" error, not a missing model. One such bench
on Robert's accounts once left no account able to serve opus.

Usage is read through the proxy's /requests/api-call, so tokens never leave the
proxy. The guard only undoes what it did itself: accounts disabled by hand, and
excluded_models entries it didn't add, are left alone.

Owner status (robert 2026-10-10) is per credential: the auth file field
"quota_owner" ("personal" = Robert's, no reserve, own tier; "coworker" = guest
tier with the FLOOR/BURN_FLOOR/0% reserve). The panel's Quota page toggles it via
PATCH /credentials/fields (proxy >= 8.0.11-robert.5 lists it). A credential
without the field (a new login) gets it seeded on the next run from the fallback
sets OWN / OWN_CODEX below; anyone not in them defaults to "coworker".

Burn switch (robert 2026-10-19): the auth file field "quota_burn" (panel Quota
page, next to the owner toggle) uses one account fully, with no floor, ahead
of Robert's own accounts but after any account about to reset (final and burn
tiers, whose quota would otherwise be lost), until its current weekly limit resets, e.g. to start
on a coworker's account early over a weekend. The panel writes true; the guard
replaces it with that weekly reset time and clears the field once the time has
passed, so the next window is back to normal (a coworker's keeps its reserve).
It does not touch Codex credits: an account at its plan limit still goes to the
credits tier as usual.

  --dry-run   print the changes without making them
  --status    print the current ordering and exit
"""
import datetime as dt
import json
import math
from zoneinfo import ZoneInfo
import os
import sys
import tempfile
import time
import urllib.error
import urllib.request

# Fallback only, for credentials without a "quota_owner" field (new logins); the
# field in the auth file is the source of truth. Not in these sets -> coworker.
OWN = {"robert@dexterity.ai", "robert.sun@dexterity.ai", "robert.sun@dex.ai"}
FLOOR = 35          # percent of the weekly limit left untouched for the account's owner
BURN_FLOOR = 5      # floor once the weekly reset is within BURN_WINDOW
BURN_WINDOW = 5 * 3600
FINAL_WINDOW = 3600  # within this of the reset, guests may be used to 0%
OWN_CODEX = {"minsoo.kim@dexterity.ai", "mark.dutka@dexterity.ai"}  # robert 2026-10-09/10: treat as his own for Codex (no floor)
OWN_TTL, GUEST_TTL = 600, 110  # usage cache lifetimes; guests are checked every run
TIER_BASE = {"final": 400, "burn": 300, "burning": 250, "own": 200, "guest": 100}
CREDITS_PRIORITY = {"own": 3, "guest": 2}  # Codex accounts billing credits; guests last
EXHAUSTED_PRIORITY = 1
NEAR_LIMIT = 85  # percent of any Codex window used before the account is re-read every run
META_REFRESH_EVERY = 600  # seconds between plan re-reads of a Meta account benched on quota

API = "http://100.106.40.3:8317/v8/management"
KEY_PATH = os.path.expanduser("~/.config/cliproxyapi-management-key")
AUTH_DIR = os.path.expanduser("~/.cli-proxy-api")
STATE = os.path.expanduser("~/.local/state/cliproxyapi-sync/quota-guard.json")
DRY_RUN = "--dry-run" in sys.argv
STATUS = "--status" in sys.argv


def mgmt(method, path, body=None):
    with open(KEY_PATH) as key_file:
        key = key_file.read().strip()
    req = urllib.request.Request(API + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def upstream_get(cred, url, headers):
    call = {"auth_index": cred["auth_index"], "method": "GET", "url": url, "header": headers}
    out = mgmt("POST", "/requests/api-call", call)
    if out.get("status_code") == 401:
        # api-call uses the stored token as-is; refresh it once and retry.
        mgmt("POST", "/credentials/refresh", {"name": cred["name"]})
        out = mgmt("POST", "/requests/api-call", call)
    if out.get("status_code") != 200:
        raise RuntimeError(f"HTTP {out.get('status_code')}: {out.get('body', '')[:200]}")
    # The management cache can serve old quota data during upstream throttling.
    # It must not be treated as a new observation when enforcing a reserve.
    headers = {k.lower(): v for k, v in (out.get("header") or {}).items()}
    if "stale" in headers.get("x-quota-cache", []):
        raise RuntimeError("quota data is stale")
    return json.loads(out["body"])


def epoch(iso):
    return int(dt.datetime.fromisoformat(iso).timestamp())


PACIFIC = ZoneInfo("America/Los_Angeles")
SESSION_RESERVE = 50
SESSION_MAX_AGE = 120


def work_hours(now):
    local = dt.datetime.fromtimestamp(now, PACIFIC)
    return local.weekday() < 5 and 8 <= local.hour < 18


def claude_session_window(usage):
    window = usage.get("five_hour")
    if window is not None:
        used, reset = window.get("utilization"), window.get("resets_at")
    else:
        window = next((w for w in usage.get("limits") or [] if w.get("kind") == "session"), None)
        if window is None:
            return None
        used, reset = window.get("percent"), window.get("resets_at")
    if isinstance(used, bool) or not isinstance(used, (int, float)) or not math.isfinite(used) or not 0 <= used <= 100:
        return None
    # An unused session legitimately has no reset time. A used session does not.
    if reset is None and used != 0:
        return None
    try:
        return [100 - used, epoch(reset) if reset else None]
    except (ValueError, TypeError):
        return None


def session_hold(kind, own, usage, now):
    if kind != "claude" or own or not work_hours(now):
        return None
    window = (usage or {}).get("session")
    if not window or now - usage.get("at", 0) > SESSION_MAX_AGE:
        return "five-hour quota unknown or stale"
    remaining, reset = window
    if reset is not None and now >= reset:
        return "five-hour reset needs fresh quota"
    if remaining <= SESSION_RESERVE:
        return f"{remaining:.0f}% five-hour quota left; reserving {SESSION_RESERVE}%"
    return None


def claude_usage(cred):
    u = upstream_get(cred, "https://api.anthropic.com/api/oauth/usage",
                     {"Authorization": "Bearer $TOKEN$", "anthropic-beta": "oauth-2025-04-20"})
    overall, scoped = None, {}
    for lim in u.get("limits") or []:
        if lim.get("kind") == "weekly_all":
            overall = [100 - lim["percent"], epoch(lim["resets_at"])]
        elif lim.get("kind") == "weekly_scoped":
            name = ((lim.get("scope") or {}).get("model") or {}).get("display_name")
            if name:
                scoped[f"claude-{name.lower()}-*"] = [100 - lim["percent"], epoch(lim["resets_at"])]
    if overall is None and u.get("seven_day"):
        overall = [100 - u["seven_day"]["utilization"], epoch(u["seven_day"]["resets_at"])]
    return overall, scoped, claude_session_window(u)


def codex_usage(cred):
    with open(os.path.join(AUTH_DIR, cred["name"])) as f:
        account_id = json.load(f)["account_id"]
    u = upstream_get(cred, "https://chatgpt.com/backend-api/wham/usage",
                     {"Authorization": "Bearer $TOKEN$", "ChatGPT-Account-Id": account_id})
    rl = u.get("rate_limit") or {}
    windows = [w for w in (rl.get("primary_window"), rl.get("secondary_window")) if w]
    # The weekly window has been reported as both primary and secondary; pick it by length.
    weekly = [w for w in windows if w.get("limit_window_seconds", 0) >= 6 * 86400]
    overall = [100 - weekly[0]["used_percent"], int(weekly[0]["reset_at"])] if weekly else None
    return overall, {}, codex_credits(u, rl, windows)


def codex_credits(u, rl, windows):
    """Credits state: billing is true while a plan limit is reached, until `until`."""
    credits = u.get("credits") or {}
    spendable = bool(credits.get("unlimited") or credits.get("has_credits")) and not (
        credits.get("overage_limit_reached") or (u.get("spend_control") or {}).get("reached"))
    reached = bool(rl.get("limit_reached")) or rl.get("allowed") is False
    full = [int(w["reset_at"]) for w in windows if w.get("used_percent", 0) >= 100 and w.get("reset_at")]
    try:
        balance = float(credits.get("balance") or 0)
    except ValueError:
        balance = 0.0
    return {"reached": reached, "until": max(full) if full else None, "spendable": spendable,
            "balance": balance, "unlimited": bool(credits.get("unlimited")),
            "near": max((w.get("used_percent", 0) for w in windows), default=0) >= NEAR_LIMIT}


def billing_credits(usage, now):
    """None (no Codex limit reached), True (on credits) or False (limit reached, no credits)."""
    c = usage.get("credits")
    if not c or not c["reached"] or (c["until"] is not None and now >= c["until"]):
        return None
    return c["spendable"]


def effective(window, now):
    """A window whose reset time has passed is assumed full until it is re-read."""
    if not window or now >= window[1]:
        return 100, None
    return window[0], window[1]


def owner_of(cred, kind, email):
    """("personal" | "coworker", seeded?) for a credential; seeded=False when the field is missing."""
    owner = (cred.get("quota_owner") or "").strip().lower()
    if owner in ("personal", "coworker"):
        return owner, True
    own = email in OWN or (kind == "codex" and email in OWN_CODEX)
    return ("personal" if own else "coworker"), False


def floor_for(own, reset, now):
    if own:
        return 0
    if reset is not None and reset - now < FINAL_WINDOW:
        return 0
    return BURN_FLOOR if reset is not None and reset - now < BURN_WINDOW else FLOOR


def burning(cred, key, reset, now):
    """Whether the account's burn switch is on. A fresh switch (true) is pinned to
    the current weekly reset; a pinned one that has passed is switched off."""
    burn = cred.get("quota_burn")
    if burn is True:
        if reset is not None and not STATUS:
            change(f"{key}: burn on, used fully until the weekly reset {when(reset)}",
                   mgmt, "PATCH", "/credentials/fields", {"name": cred["name"], "quota_burn": int(reset)})
            cred["quota_burn"] = int(reset)
        return True
    if isinstance(burn, (int, float)) and not isinstance(burn, bool):
        if now < burn:
            return True
        if not STATUS:
            change(f"{key}: weekly limit reset {when(burn)}; burn off, back to normal",
                   mgmt, "PATCH", "/credentials/fields", {"name": cred["name"], "quota_burn": None})
        return False
    return False


def set_guard_excludes(cred, old, new):
    path = os.path.join(AUTH_DIR, cred["name"])
    with open(path) as f:
        data = json.load(f)
    keep = [m for m in data.get("excluded_models") or [] if m not in old]
    data["excluded_models"] = keep + sorted(new)
    if not data["excluded_models"]:
        del data["excluded_models"]
    fd, tmp = tempfile.mkstemp(dir=AUTH_DIR, prefix=".guard-")
    with os.fdopen(fd, "w") as f:
        json.dump(data, f, indent=2)
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)


def meta_tier(cred):
    """The Meta subscription tier recorded in the credential file, or None."""
    try:
        with open(os.path.join(AUTH_DIR, cred["name"])) as f:
            data = json.load(f)
    except (OSError, ValueError, KeyError):
        return None
    return data.get("subs_tier_id"), data.get("subs_tier_name")


def unbench_upgraded_meta(cred, state, now):
    """A Meta account benched on quota stays benched until the old plan's window
    resets, even after a plan upgrade. Re-read the plan now and then and clear the
    cooldown as soon as the tier differs from the one it was benched under."""
    key = f"meta:{(cred.get('email') or cred.get('name') or '').lower()}"
    entry = state.setdefault(key, {})
    benched = cred.get("unavailable") and any(
        c.get("reason") in ("credential_quota", "quota") for c in cred.get("cooldowns") or [])
    if not benched:
        entry.pop("benched_tier", None)
        entry["tier"] = meta_tier(cred)
        return
    entry.setdefault("benched_tier", entry.get("tier") or meta_tier(cred))
    if now - entry.get("refreshed_at", 0) < META_REFRESH_EVERY or STATUS:
        return
    entry["refreshed_at"] = now
    if DRY_RUN:
        print(f"{key}: would re-read the plan")
        return
    mgmt("POST", "/credentials/refresh", {"name": cred["name"]})
    tier = meta_tier(cred)
    if tier and entry["benched_tier"] and tier[0] != entry["benched_tier"][0]:
        change(f"{key}: plan changed {entry['benched_tier'][1]} -> {tier[1]}; clearing quota cooldown",
               mgmt, "POST", "/routing/cooldown/reset", {"auth_index": cred["auth_index"]})
        entry.pop("benched_tier", None)
    entry["tier"] = tier


def change(desc, fn, *args):
    print(desc)
    if not DRY_RUN:
        fn(*args)


def when(ts):
    return dt.datetime.fromtimestamp(ts).strftime("%a %m-%d %H:%M") if ts else "-"


def main():
    try:
        with open(STATE) as f:
            state = json.load(f)
    except (OSError, ValueError):
        state = {}
    now = time.time()
    creds = mgmt("GET", "/credentials")
    creds = creds.get("files") or creds.get("credentials") or creds

    for cred in creds:
        if (cred.get("type") or cred.get("provider")) != "claude" or cred.get("disabled"):
            continue
        benched = [c for c in cred.get("cooldowns") or [] if c.get("reason") == "not_found"]
        if benched and not STATUS:
            models = ", ".join(c.get("model_key") or "all models" for c in benched)
            change(f"claude:{cred.get('email')}: clearing 404 cooldown on {models}",
                   mgmt, "POST", "/routing/cooldown/reset", {"auth_index": cred["auth_index"]})

    for cred in creds:
        if (cred.get("type") or cred.get("provider")) == "meta" and not cred.get("disabled"):
            try:
                unbench_upgraded_meta(cred, state, now)
            except (urllib.error.URLError, OSError) as error:
                print(f"meta:{cred.get('email')}: plan re-read failed: {error}")

    accounts = []
    for cred in creds:
        kind, email = cred.get("type") or cred.get("provider"), (cred.get("email") or "").lower()
        if kind not in ("claude", "codex") or not email:
            continue
        key = f"{kind}:{email}"
        owner, seeded = owner_of(cred, kind, email)
        own = owner == "personal"
        cred["_owner"] = owner
        if not seeded and not STATUS:
            change(f"{key}: quota_owner unset -> {owner} (default)",
                   mgmt, "PATCH", "/credentials/fields", {"name": cred["name"], "quota_owner": owner})
        st = state.setdefault(key, {})
        if cred.get("disabled") and not st.get("guard_disabled"):
            continue  # disabled by hand; not ours to manage

        cached = st.get("usage")
        last = (cached or {}).get("credits") or {}
        near = last.get("reached") or last.get("near")
        ttl = OWN_TTL if own and not near else GUEST_TTL
        if kind == "claude" and not own and work_hours(now):
            ttl = 30  # The proxy coalesces upstream usage reads in its shared cache.
        stale = (not cached or now - cached["at"] > ttl or (cached["overall"] and now >= cached["overall"][1])
                 or (kind == "codex" and "credits" not in cached)
                 or (kind == "claude" and ("session" not in cached or
                     (cached.get("session") and cached["session"][1] is not None and now >= cached["session"][1]))))
        # Recheck guard-disabled accounts too: their five-hour window can reset
        # before the weekly window. upstream_get refreshes expired tokens once.
        if stale and not STATUS:
            try:
                if kind == "claude":
                    overall, scoped, session = claude_usage(cred)
                    credits = None
                else:
                    overall, scoped, credits = codex_usage(cred)
                    session = None
                st["usage"] = cached = {"overall": overall, "scoped": scoped, "credits": credits, "session": session, "at": now}
            except Exception as err:
                print(f"{key}: usage check failed ({err}); using last known usage")
        if not cached:
            if kind != "claude" or own or not work_hours(now):
                continue
            st["usage"] = {"overall": None, "scoped": {}, "credits": None, "session": None, "at": 0}
        accounts.append((cred, key, own, st))

    for cred, key, own, st in accounts:
        cred["_burn"] = burning(cred, key, effective(st["usage"]["overall"], now)[1], now)

    # The proxy benches a Codex account for "quota" until the reset time in the 429, and keeps
    # that bench even when the window resets early (manual reset, reset moved). Clear it once
    # fresh usage shows the account is not at a limit any more.
    for cred, key, own, st in accounts:
        if (cred.get("type") or cred.get("provider")) != "codex" or STATUS:
            continue
        usage = st["usage"]
        remaining, _ = effective(usage["overall"], now)
        fresh = now - usage["at"] < GUEST_TTL + 10
        benched = [c for c in cred.get("cooldowns") or [] if c.get("reason") == "quota"]
        if benched and fresh and remaining >= 1 and billing_credits(usage, now) is None:
            models = ", ".join(sorted({c.get("model_key") or "all models" for c in benched}))
            change(f"{key}: {remaining:.0f}% weekly left; clearing stale quota cooldown on {models}",
                   mgmt, "POST", "/routing/cooldown/reset", {"auth_index": cred["auth_index"]})

    # Floors: enable/disable and scoped exclusions (guest accounts only).
    for cred, key, own, st in accounts:
        remaining, reset = effective(st["usage"]["overall"], now)
        burn = cred["_burn"]
        floor = 0 if burn else floor_for(own, reset, now)
        # A guest at its plan limit with credits is only billing credits (last resort).
        want_off = not own and not burn and remaining <= floor and not billing_credits(st["usage"], now)
        hold = session_hold(cred.get("type") or cred.get("provider"), own, st["usage"], now)
        want_off = want_off or hold is not None
        st["session_hold"] = hold
        if want_off != bool(cred.get("disabled")) and not STATUS:
            verb = (f"disabled until {when(reset)}" if want_off else
                    "re-enabled (plan limit reached; credits only, last resort)" if billing_credits(st["usage"], now)
                    else "re-enabled")
            change(f"{key}: {hold or f'{remaining:.0f}% weekly left, floor {floor}%'} -> {verb}",
                   mgmt, "PATCH", "/credentials/status", {"name": cred["name"], "disabled": want_off})
            cred["disabled"] = want_off
        st["guard_disabled"] = want_off

        have = st.get("excluded", {})
        want = {}
        for glob, window in (st["usage"]["scoped"] or {}).items():
            rem, rst = effective(window, now)
            if not own and not burn and rem <= floor_for(own, rst, now):
                want[glob] = rst
        if set(want) != set(have) and not STATUS:
            desc = "; ".join([f"exclude {g} until {when(want[g])}" for g in set(want) - set(have)] +
                             [f"un-exclude {g}" for g in set(have) - set(want)])
            change(f"{key}: {desc}", set_guard_excludes, cred, set(have), set(want))
        st["excluded"] = want

    # Priorities: tier, then soonest reset first, within each provider.
    rows = []
    for kind in ("claude", "codex"):
        group = [a for a in accounts if a[0].get("type", a[0].get("provider")) == kind]
        tiers = {}
        for cred, key, own, st in group:
            remaining, reset = effective(st["usage"]["overall"], now)
            billing = billing_credits(st["usage"], now)
            if billing:
                tier = "credits"
            elif billing is False or ((own or cred["_burn"]) and remaining < 1):
                tier = "exhausted"
            elif reset is not None and reset - now < FINAL_WINDOW:
                tier = "final"
            elif reset is not None and reset - now < BURN_WINDOW:
                tier = "burn"
            elif cred["_burn"]:
                tier = "burning"
            else:
                tier = "own" if own else "guest"
            cred["_balance"] = (st["usage"].get("credits") or {}).get("balance", 0.0)
            tiers.setdefault(tier, []).append((reset if reset is not None else float("inf"), -remaining, key, cred, remaining, reset, own))
        for tier, members in tiers.items():
            if tier == "credits":  # Robert's first, then the largest balance
                members.sort(key=lambda m: (not m[6], -m[3]["_balance"]))
            else:
                members.sort()
            for rank, (_, _, key, cred, remaining, reset, own) in enumerate(members):
                if tier == "credits":
                    prio = CREDITS_PRIORITY["own" if own else "guest"]
                elif tier == "exhausted":
                    prio = EXHAUSTED_PRIORITY
                else:
                    prio = TIER_BASE[tier] + len(members) - rank
                if cred.get("priority") != prio and not STATUS:
                    change(f"{key}: priority {cred.get('priority')} -> {prio} ({tier}, resets {when(reset)})",
                           mgmt, "PATCH", "/credentials/fields", {"name": cred["name"], "priority": prio})
                credits = state[key]["usage"].get("credits")
                owner = cred["_owner"] + ("+burn" if cred["_burn"] else "")
                floor = 0 if cred["_burn"] else floor_for(own, reset, now)
                rows.append((kind, -prio, key, owner, prio, tier, remaining, reset, floor, cred.get("disabled"),
                             "-" if not credits else "unlimited" if credits["unlimited"] else f"{credits['balance']:,.0f}"))

    if STATUS:
        print(f"{'account':42} {'owner':13} {'prio':>4}  {'tier':9} {'left':>5}  {'floor':>5}  {'weekly reset':15} {'state':5} credits")
        for kind, _, key, owner, prio, tier, remaining, reset, floor, disabled, credits in sorted(rows):
            print(f"{key:42} {owner:13} {prio:>4}  {tier:9} {remaining:>4.0f}%  {floor:>4}%  {when(reset):15} {'OFF' if disabled else 'on':5} {credits}")
        for cred, key, own, st in accounts:
            if (cred.get("type") or cred.get("provider")) == "claude" and not own:
                window = st["usage"].get("session")
                left = f"{window[0]:.0f}%" if window else "unknown"
                print(f"{key}: five-hour left {left}; work-hours reserve "
                      f"{'active' if work_hours(now) else 'inactive'}; "
                      f"{st.get('session_hold') or 'no session hold'}")
        return 0

    if not DRY_RUN:
        os.makedirs(os.path.dirname(STATE), mode=0o700, exist_ok=True)
        with open(STATE, "w") as f:
            json.dump(state, f, indent=2)
    return 0


if __name__ == "__main__":
    sys.exit(main())
