#!/usr/bin/env python3
"""Offline replay of tacit_search (eval/VALUE.md, "Search, the push and the
tree"): each case's request as an agent's query, through `tacit mcp` in the
case's cwd and a throwaway HOME, against a copy of the store; graded with
repo_first.py's judge and cache (case|fact), so a line graded for a hook block
is not graded again. No model calls except the cached judge.

  search_replay.py run   --exe tacit --config config.yaml --cases cases.json --out o.json [--env K=V ...]
  search_replay.py judge --cases cases.json --out o.json --cache judge.json
  search_replay.py table --cases cases.json --cache judge.json o.json [o2.json ...]

Cases are repo_first.py's: fact cases (holdout) count a case reached when a
line states the truth fact (judge: yes); items cases (design) count items
whose full carrier is a line. Noise is the judge's share of lines graded noise.
"""
import argparse, concurrent.futures as cf, json, os, re, shutil, subprocess, sys, tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import repo_first  # noqa: E402  (judge prompt, claude call)

LINE = repo_first.LINE


def search(exe, config, cwd, query, env):
    """One tacit_search call: initialize, initialized, tools/call."""
    home = tempfile.mkdtemp(prefix='tacit-search-')
    p = subprocess.Popen([exe, '--config', config, 'mcp', '--harness', 'claude'], cwd=cwd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                         stderr=subprocess.DEVNULL, text=True, env=dict(os.environ, HOME=home, TACIT_SETTINGS=config, **env))
    msgs = [{'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocolVersion': '2025-06-18', 'capabilities': {}, 'clientInfo': {'name': 'replay', 'version': '0'}}},
            {'jsonrpc': '2.0', 'method': 'notifications/initialized', 'params': {}},
            {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/call', 'params': {'name': 'tacit_search', 'arguments': {'query': query}}}]
    out = ''
    try:
        for m in msgs:
            p.stdin.write(json.dumps(m) + '\n')
            p.stdin.flush()
            if 'id' not in m:
                continue
            while True:
                r = json.loads(p.stdout.readline())
                if r.get('id') == m['id']:
                    break
        out = '\n'.join(c.get('text', '') for c in (r.get('result') or {}).get('content') or [])
    finally:
        p.stdin.close()
        p.wait(timeout=30)
        shutil.rmtree(home, ignore_errors=True)
    return out


def run(a):
    cases = json.load(open(a.cases))
    env = dict(kv.split('=', 1) for kv in a.env or [])
    with cf.ThreadPoolExecutor(a.jobs) as ex:
        res = dict(zip([c['id'] for c in cases], ex.map(lambda c: search(a.exe, a.config, c['cwd'], c['prompt'], env), cases)))
    json.dump(res, open(a.out, 'w'), indent=1)
    print('cases', len(res))


def judge(a):
    # repo_first.judge reads {setting: {case: {ss, prompt, tools}}}: put each answer in the prompt slot
    res = json.load(open(a.out))
    tmp = a.out + '.as-blocks.json'
    json.dump({'search': {cid: {'ss': '', 'prompt': t, 'tools': []} for cid, t in res.items()}}, open(tmp, 'w'))
    a.out = tmp
    repo_first.judge(a)
    os.remove(tmp)


def table(a):
    cache = json.load(open(a.cache)) if os.path.exists(a.cache) else {}
    cases = {c['id']: c for c in json.load(open(a.cases))}
    print(f"{'answers':32} | {'holdout reached':>15} {'partial':>7} {'lines':>5} {'noise':>5} | {'design items':>12} {'noise':>5} | {'chars':>6}")
    for o in a.outs:
        res = json.load(open(o))
        h = dict(n=0, r=0, p=0, l=0, nz=0, g=0)
        d = dict(n=0, r=0, nz=0, g=0)
        chars = 0
        for cid, text in res.items():
            c, names = cases[cid], [n for n, _ in LINE.findall(text)]
            chars += len(text)
            g = [cache.get(f'{cid}|{n}') for n in names]
            if 'items' in c:
                d['n'] += 1
                d['r'] += sum(1 for it in c['items'].values() if set(it['full']) & set(names))
                d['g'] += sum(x is not None for x in g)
                d['nz'] += sum(1 for x in g if x and x['use'] == 'noise')
            else:
                st = [x['states'] for x in g if x]
                h['n'] += 1
                h['l'] += len(names)
                h['r'] += 'yes' in st
                h['p'] += 'yes' not in st and 'partial' in st
                h['g'] += sum(x is not None for x in g)
                h['nz'] += sum(1 for x in g if x and x['use'] == 'noise')
        print(f"{os.path.basename(o):32} | {h['r']:15} {h['p']:7} {h['l']/max(h['n'],1):5.1f} {100*h['nz']/max(h['g'],1):4.0f}% | {d['r']:12} {100*d['nz']/max(d['g'],1):4.0f}% | {chars/max(len(res),1):6.0f}")


if __name__ == '__main__':
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('cmd', choices=['run', 'judge', 'table'])
    ap.add_argument('outs', nargs='*')
    ap.add_argument('--exe', default='tacit')
    ap.add_argument('--config', default=os.path.expanduser('~/.config/tacit/config.yaml'))
    ap.add_argument('--cases', required=True)
    ap.add_argument('--out')
    ap.add_argument('--cache')
    ap.add_argument('--env', action='append', help='K=V for the server (repeatable)')
    ap.add_argument('--model', default=os.environ.get('TACIT_JUDGE_MODEL', 'sonnet'))
    ap.add_argument('--claude-config', default=os.environ.get('TACIT_JUDGE_CLAUDE_CONFIG'))
    ap.add_argument('--jobs', type=int, default=8)
    a = ap.parse_args()
    a.exe = os.path.abspath(shutil.which(a.exe) or a.exe)
    a.config = os.path.abspath(os.path.expanduser(a.config))
    {'run': run, 'judge': judge, 'table': table}[a.cmd](a)
