"""Pinned official Jev request helpers and synthetic-only smoke CLI.

No calls on import. Hosted calls require explicit --allow-egress. Never run this
on private input without its owner's approval. No secrets are logged.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import shlex
import sys
import urllib.error
import urllib.request

MODEL = 'jev-1.13.0'
URL = 'https://api.typesafe.ai/v1/systemone'
BASELINE = 'Does this memory add concrete information or a constraint useful for fulfilling the current request, beyond what the request already states? Mere topic overlap is not sufficient.'
CRITERIA = {
    'true': 'The memory provides an additional actionable fact, constraint, preference, or procedure that is relevant to fulfilling the task. It is not already stated in the task.',
    'false': 'The memory only repeats or paraphrases the task, shares its topic without helping, or suggests an inappropriate or contradictory action.',
}
CHOICES = {
    'useful': 'Additional actionable information or a relevant constraint that helps fulfill the task.',
    'redundant': 'Only repeats or paraphrases information already contained in the task.',
    'not_useful': 'Topic overlap, unrelated background, or an inappropriate or contradictory action.',
}


def get_key(path=None):
    key = os.environ.get('TYPESAFE_API_KEY', '').strip()
    if not key and path:
        for line in Path(path).expanduser().read_text().splitlines():
            name, sep, value = line.strip().removeprefix('export ').partition('=')
            if sep and name.strip() == 'TYPESAFE_API_KEY':
                parts = shlex.split(value, comments=True)
                if len(parts) == 1:
                    key = parts[0]
    if not key or '\n' in key or '\r' in key:
        raise ValueError('Set TYPESAFE_API_KEY or supply --key-file; never put a key in command arguments')
    return key


def payload(row):
    questions = {}
    for i, candidate in enumerate(row['candidates']):
        instruction = {'memory': candidate['text'], 'question': BASELINE}
        questions[f'baseline_{i}'] = {'type': 'noul', 'instructions': instruction}
        questions[f'rubric_{i}'] = {'type': 'noul', 'instructions': instruction, 'criteria': CRITERIA}
        questions[f'choice_{i}'] = {
            'type': 'choice',
            'instructions': {'memory': candidate['text'], 'question': 'How does this memory contribute to fulfilling the task? The task itself is already known.'},
            'criteria': CHOICES,
        }
    return {'model': MODEL, 'state': {'task': row['query']}, 'questions': questions}


def object_hash(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(',', ':')).encode()).hexdigest()


def validate_response(data, body):
    if data.get('model') != MODEL or set(data.get('answers', {})) != set(body['questions']):
        raise ValueError('Response model or question IDs differ from request')
    for key, answer in data['answers'].items():
        if answer.get('type') != body['questions'][key]['type']:
            raise ValueError('Response answer type mismatch')
        if answer['type'] == 'noul':
            values = [answer.get('noul')]
        else:
            probs = answer.get('probabilities', {})
            if set(probs) != set(CHOICES) or answer.get('choice') not in CHOICES:
                raise ValueError('Choice schema mismatch')
            values = list(probs.values())
            if not all(isinstance(v, (int, float)) for v in values) or abs(sum(values)-1) > .02:
                raise ValueError('Choice probabilities do not sum to one')
        if not all(isinstance(v, (int, float)) and math.isfinite(v) and 0 <= v <= 1 for v in values):
            raise ValueError('Invalid probability')
    tokens = data.get('usage', {}).get('input_tokens')
    if not isinstance(tokens, int) or not 0 <= tokens <= 64000:
        raise ValueError('Invalid usage or excessive token length')


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request(body, key, timeout=60):
    state_size = len(json.dumps(body['state'], ensure_ascii=False).encode())
    question_sizes = [len(json.dumps(q, ensure_ascii=False).encode()) for q in body['questions'].values()]
    if state_size + max(question_sizes, default=0) >= 32000 or state_size + sum(question_sizes) >= 64000:
        raise ValueError('Input exceeds conservative byte budget; never truncated')
    req = urllib.request.Request(URL, data=json.dumps(body).encode(), headers={
        'Content-Type': 'application/json', 'Authorization': 'Bearer ' + key}, method='POST')
    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=timeout) as response:
            raw = response.read(1048577)
            if len(raw) > 1048576:
                raise ValueError('Response too large')
            data = json.loads(raw)
    except urllib.error.HTTPError as error:
        raise RuntimeError(f'Official Jev request failed: HTTP {error.code}; response body withheld') from None
    except urllib.error.URLError:
        raise RuntimeError('Official Jev connection failed; transport details withheld') from None
    validate_response(data, body)
    return data


def main():
    from jev_devcal import run
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, help='external private output directory')
    parser.add_argument('--key-file', help='optional private dotenv file, environment key wins')
    parser.add_argument('--allow-egress', action='store_true', help='authorize official TypeSafe calls on synthetic inputs')
    parser.add_argument('--repeats', type=int, default=3)
    parser.add_argument('--timeout', type=float, default=60)
    args = parser.parse_args()
    args.input = str(Path(__file__).resolve().parents[1] / 'fixtures' / 'smoke.jsonl')
    run(args)


if __name__ == '__main__':
    main()
