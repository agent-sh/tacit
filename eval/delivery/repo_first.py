#!/usr/bin/env python3
"""Offline replay of the shipped hooks for delivery settings (eval/VALUE.md,
"Repo-first delivery"). For every case it runs, in a throwaway HOME and one
session: `tacit hook session-start --mcp`, `tacit hook prompt` with the
case's prompt, then `tacit hook pre-tool` for each of the case's tool calls
(each list of calls in its own copy of that session). No model calls, except
the cached judge below.

Case file (JSON list), one object per case:
  id, cwd, prompt
  items      {item: {"full": [fact names], "partial": [...]}}   (design cases)
  fact       the one fact the situation needs, as text             (holdout cases)
  tool_calls [[{"tool_name": "Edit", "tool_input": {...}}, ...], ...]

Settings file: {"name": {"TACIT_DELIVERY_...": "on", ..., "_config": "<config.yaml>"}}.
Every TACIT_DELIVERY_* switch not named is off, so {} is the release before
these settings.

  repo_first.py run --exe tacit --config config.yaml --cases cases.json --settings s.json --out o.json
  repo_first.py judge --cases cases.json --out o.json --cache judge.json   (claude -p, cached per case and line)
  repo_first.py table --cases cases.json --out o.json --cache judge.json

Measures per setting: needed items whose full carrier reached the agent at
SessionStart or the prompt (items cases) or truth facts stated (fact cases),
plus those reached only at a tool call; characters and lines per case of
the SessionStart and prompt blocks; the judge's noise share of those lines;
tool-call lines per case.
"""
import argparse, concurrent.futures as cf, json, os, re, shutil, subprocess, tempfile, threading, uuid

NAME = re.compile(r'^- (?:RULE )?([a-z0-9][a-z0-9-]*)', re.M)
LINE = re.compile(r'^- (?:RULE )?([a-z0-9][a-z0-9-]*)[^:\n]*: (.*)$', re.M)
SWITCHES = ['REPO_START', 'REPO_ALIASES', 'PROMPT_METADATA', 'PROMPT_ACTION', 'INSTRUCTION', 'TOOL_PUSH']


def hook(exe, config, sub, payload, env, cwd, flags=()):
    p = subprocess.run([exe, '--config', config, 'hook', sub, *flags], input=json.dumps(payload), capture_output=True, text=True, env=env, cwd=cwd, timeout=60)
    return p.stdout


def replay(exe, config, case, setting):
    setting = dict(setting)
    config = setting.pop('_config', config)
    home = tempfile.mkdtemp(prefix='tacit-replay-')
    env = dict(os.environ, HOME=home, **{'TACIT_DELIVERY_' + k: 'off' for k in SWITCHES})
    env.update(setting)
    sid = str(uuid.uuid4())
    ss = hook(exe, config, 'session-start', {'session_id': sid, 'cwd': case['cwd'], 'source': 'startup'}, env, case['cwd'], ['--mcp'])
    pr = hook(exe, config, 'prompt', {'session_id': sid, 'cwd': case['cwd'], 'prompt': case['prompt']}, env, case['cwd'])
    sessions = os.path.join(home, '.local/state/tacit/sessions')
    tools = []
    for calls in case.get('tool_calls') or []:
        sid2 = str(uuid.uuid4())
        if os.path.exists(os.path.join(sessions, sid)):
            shutil.copy(os.path.join(sessions, sid), os.path.join(sessions, sid2))
        got = []
        for i, call in enumerate(calls):
            out = hook(exe, config, 'pre-tool', {'session_id': sid2, 'cwd': case['cwd'], 'hook_event_name': 'PreToolUse', **call}, env, case['cwd'])
            if out.strip():
                got.append({'at': i, 'tool': call['tool_name'], 'block': json.loads(out)['hookSpecificOutput']['additionalContext']})
        tools.append(got)
    shutil.rmtree(home, ignore_errors=True)
    return {'ss': ss, 'prompt': pr, 'tools': tools}


def run(a):
    cases = json.load(open(a.cases))
    settings = json.load(open(a.settings))
    res = json.load(open(a.out)) if os.path.exists(a.out) else {}
    for name, setting in settings.items():
        if name in res:
            continue
        with cf.ThreadPoolExecutor(a.jobs) as ex:
            rows = dict(zip([c['id'] for c in cases], ex.map(lambda c: replay(a.exe, a.config, c, setting), cases)))
        res[name] = rows
        json.dump(res, open(a.out, 'w'))
        print('done', name, flush=True)


def claude(prompt, model, cfg):
    env = dict(os.environ)
    if cfg:
        env['CLAUDE_CONFIG_DIR'] = cfg
    p = subprocess.run(['claude', '-p', prompt, '--model', model, '--output-format', 'json', '--tools', '', '--max-turns', '1', '--setting-sources', 'user', '--strict-mcp-config'],
                       capture_output=True, text=True, env=env, timeout=300)
    d = json.loads(p.stdout)
    return d['result'], d.get('total_cost_usd') or 0


def judge(a):
    res = json.load(open(a.out))
    cache = json.load(open(a.cache)) if os.path.exists(a.cache) else {}
    cases = {c['id']: c for c in json.load(open(a.cases))}
    todo = {}
    for rows in res.values():
        for cid, r in rows.items():
            text = r['ss'] + r['prompt'] + ''.join(t['block'] for calls in r['tools'] for t in calls)
            for n, d in LINE.findall(text):
                if f'{cid}|{n}' not in cache:
                    todo.setdefault(cid, {})[n] = d
    print('cases', len(todo), 'lines', sum(len(v) for v in todo.values()), flush=True)
    lock, cost = threading.Lock(), [0.0]

    def one(cid):
        c, items = cases[cid], list(todo[cid].items())
        listing = '\n'.join(f'{i+1}. {n}: {d}' for i, (n, d) in enumerate(items))
        if 'items' in c:
            prompt = (f"A coding agent working in a checkout got this request:\n\n{c['prompt']}\n\nA memory tool may show it some of these notes. Grade each note for THIS request only:\n"
                      "needed = the agent is likely to get the request wrong without it; useful = helps but not required; noise = no help for this request.\n"
                      'Reply with one JSON object only, mapping the note number to its grade, e.g. {"1": "noise", "2": "needed"}.\n\n' + listing)
        else:
            prompt = (f"A coding agent is in this situation:\n\n{c['prompt']}\n\nThe agent needs this fact: {c['fact']}\n\nA memory tool may show it some of these notes. For each note give two grades:\n"
                      '- "states": does the note state the fact, or its actionable core (the specific cause, rule, value, path or fix)? yes / partial / no. Same topic without the fact is no.\n'
                      '- "use": for this situation, needed (the agent likely goes wrong without it), useful (helps, not required) or noise (no help).\n'
                      'Reply with one JSON object only, mapping the note number to [states, use], e.g. {"1": ["no", "noise"], "2": ["yes", "needed"]}.\n\n' + listing)
        try:
            txt, usd = claude(prompt, a.model, a.claude_config)
            g = json.loads(re.search(r'\{.*\}', txt, re.S).group(0))
        except Exception as e:  # a failed judgement stays ungraded and is retried next run
            print('judge failed', cid, e, flush=True)
            return
        with lock:
            cost[0] += usd
            for i, (n, _) in enumerate(items):
                v = g.get(str(i + 1))
                if v is not None:
                    cache[f'{cid}|{n}'] = {'use': v} if 'items' in c else {'states': v[0], 'use': v[1]}
    with cf.ThreadPoolExecutor(8) as ex:
        list(ex.map(one, list(todo)))
    json.dump(cache, open(a.cache, 'w'), indent=0)
    print('judge cost', round(cost[0], 3))


def table(a):
    res = json.load(open(a.out))
    cache = json.load(open(a.cache)) if a.cache and os.path.exists(a.cache) else {}
    cases = {c['id']: c for c in json.load(open(a.cases))}
    print(f"{'setting':34} | {'reached':>7} {'+tool':>5} {'partial':>7} | {'chars/case':>10} {'lines/case':>10} {'noise':>6} | {'tool lines/case':>15}")
    for name, rows in res.items():
        if a.only and name not in a.only:
            continue
        n = reached = tool = part = chars = lines = noise = graded = tlines = 0
        for cid, r in rows.items():
            c = cases[cid]
            push = r['ss'] + r['prompt']
            got = set(NAME.findall(push))
            tool_blocks = [set(NAME.findall(t['block'])) for calls in r['tools'] for t in calls]
            tgot = set().union(*tool_blocks) if tool_blocks else set()
            n += 1
            chars += len(push)
            lines += len(got)
            tlines += sum(len(b) for b in tool_blocks)
            for g in (cache.get(f'{cid}|{x}', {}).get('use') for x in got):
                graded += g is not None
                noise += g == 'noise'
            if 'items' in c:
                for facts in c['items'].values():
                    if set(facts['full']) & got:
                        reached += 1
                    elif set(facts['full']) & tgot:
                        tool += 1
                    elif set(facts.get('partial') or []) & got:
                        part += 1
            else:
                st = [cache.get(f'{cid}|{x}', {}).get('states') for x in got]
                if 'yes' in st:
                    reached += 1
                elif 'yes' in [cache.get(f'{cid}|{x}', {}).get('states') for x in tgot - got]:
                    tool += 1
                elif 'partial' in st:
                    part += 1
        print(f"{name:34} | {reached:7} {tool:5} {part:7} | {chars/n:10.0f} {lines/n:10.1f} {100*noise/max(1,graded):5.0f}% | {tlines/n:15.2f}")


if __name__ == '__main__':
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('cmd', choices=['run', 'judge', 'table'])
    ap.add_argument('--exe', default='tacit')
    ap.add_argument('--config', default=os.path.expanduser('~/.config/tacit/config.yaml'))
    ap.add_argument('--cases', required=True)
    ap.add_argument('--settings')
    ap.add_argument('--out', required=True)
    ap.add_argument('--cache')
    ap.add_argument('--model', default=os.environ.get('TACIT_JUDGE_MODEL', 'sonnet'))
    ap.add_argument('--claude-config', default=os.environ.get('TACIT_JUDGE_CLAUDE_CONFIG'))
    ap.add_argument('--jobs', type=int, default=8)
    ap.add_argument('--only', nargs='*')
    a = ap.parse_args()
    # Hooks run in each case's cwd: resolve paths first.
    a.exe = os.path.abspath(shutil.which(a.exe) or a.exe)
    a.config = os.path.abspath(os.path.expanduser(a.config))
    {'run': run, 'judge': judge, 'table': table}[a.cmd](a)
