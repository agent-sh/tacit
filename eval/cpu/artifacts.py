"""Opt-in pinned public downloads. No workload data is sent to the network."""
import argparse
import fnmatch
import hashlib
import json
from pathlib import Path
import urllib.request
from common import add_workspace, digest, workspace

PINS = {
    'qwen': {
        '0.6B': ('Qwen/Qwen3-Reranker-0.6B', 'e61197ed45024b0ed8a2d74b80b4d909f1255473'),
        '4B': ('Qwen/Qwen3-Reranker-4B', '22e683669bc0f0bd69640a1354a6d0aebcfeede5'),
    },
    'laya': {
        'base': ('convaiinnovations/laya', '7b928d828b7b0e022f929d9bd2e44165aa270148'),
        'typed': ('convaiinnovations/laya-typed-decisions', 'e929ae5cf69bc34259cd2f95c9e91145b818b1f0'),
    },
    'jina': {'v3.5': ('jinaai/jina-reranker-v3.5', 'e8a93f33f0b22108f8c2364f8484ce3422552fbc')},
    'controls': {
        'minilm': ('cross-encoder/ms-marco-MiniLM-L6-v2', '233902d25c440f23af6f7d6e94d2946bac0bee0a'),
        'tiny': ('cross-encoder/ms-marco-TinyBERT-L2-v2', '81d1926f67cb8eee2c2be17ca9f793c7c3bd20cc'),
    },
}
SOURCE_SHA256 = 'ec6612461b4307eb3bab089e6916c00881b7e6b2e8edfbd56a9ace4560244837'
CONTROL_FILES = ['onnx/model.onnx', 'tokenizer.json', 'tokenizer_config.json', 'special_tokens_map.json', 'vocab.txt', 'config.json', 'README.md']


def patterns(group):
    if group == 'controls':
        return CONTROL_FILES
    if group == 'laya':
        return ['rl_agent_config.json', 'model.safetensors', 'tokenizer/*', 'encoder/*']
    result = ['*.json', '*.safetensors', '*.txt', '*.model', 'README.md', '*.jinja']
    if group == 'jina':
        result.append('modeling.py')
    return result


def download(group, key, root):
    repo, revision = PINS[group][key]
    if group == 'controls':
        root = root / key
    root.mkdir(parents=True, exist_ok=True)
    model = root / ('model-' + key if group == 'qwen' else 'model')
    url = f'https://huggingface.co/api/models/{repo}/revision/{revision}?blobs=true'
    with urllib.request.urlopen(url, timeout=45) as response:
        metadata = json.load(response)
    if metadata['sha'] != revision or metadata['id'] != repo:
        raise ValueError('upstream repository/revision mismatch')
    records = {}
    for blob in metadata['siblings']:
        name = blob['rfilename']
        if not any(fnmatch.fnmatchcase(name, pattern) for pattern in patterns(group)):
            continue
        path = (model / name).resolve()
        if model.resolve() not in path.parents:
            raise ValueError('unsafe artifact path')
        path.parent.mkdir(parents=True, exist_ok=True)
        artifact_url = f'https://huggingface.co/{repo}/resolve/{revision}/{name}'
        pending = path.with_name(path.name + '.partial')
        with urllib.request.urlopen(artifact_url, timeout=90) as response, pending.open('wb') as stream:
            while chunk := response.read(1024 * 1024):
                stream.write(chunk)
        sha = digest(pending)
        if 'size' in blob and pending.stat().st_size != blob['size']:
            raise ValueError('size mismatch: ' + name)
        if blob.get('lfs'):
            if sha != blob['lfs']['sha256']:
                raise ValueError('LFS SHA256 mismatch: ' + name)
        else:
            h = hashlib.sha1(b'blob ' + str(pending.stat().st_size).encode() + b'\0')
            with pending.open('rb') as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                    h.update(chunk)
            if h.hexdigest() != blob['blobId']:
                raise ValueError('Git blob mismatch: ' + name)
        if group == 'jina' and name == 'modeling.py' and sha != SOURCE_SHA256:
            raise ValueError('reviewed Jina source hash changed')
        pending.replace(path)
        records[name] = {'sha256': sha, 'bytes': path.stat().st_size, 'url': artifact_url}
    if not records or (group == 'jina' and 'modeling.py' not in records):
        raise ValueError('missing artifacts')
    manifest = {'repository': repo, 'repo': repo, 'revision': revision, 'files': records}
    if group == 'qwen':
        manifest['files'] = [{'name': name, **item} for name, item in records.items()]
        target = root / ('manifest-' + key + '.json')
    elif group == 'laya':
        manifest.update(path=str(model), sha256={name: r['sha256'] for name, r in records.items()})
        target = root / (key + '-artifacts.json')
        # Base and typed checkpoints cannot share the same model directory.
    else:
        target = root / 'manifest.json'
    target.write_text(json.dumps(manifest, indent=2) + '\n')
    print(json.dumps({'manifest': str(target), 'files': len(records), 'revision': revision}))


def main(group):
    p = argparse.ArgumentParser(description='Explicit pinned artifact download; never loads model code. Jina is noncommercial research only.')
    add_workspace(p)
    p.add_argument('--model', choices=PINS[group], required=True)
    a = p.parse_args()
    root = workspace(group, a.workspace)
    # Separate Laya checkpoints, while keeping receipts at the group root.
    if group == 'laya':
        download_laya(a.model, root)
    else:
        download(group, a.model, root)


def download_laya(key, root):
    isolated = root / key
    download('laya', key, isolated)
    root.mkdir(parents=True, exist_ok=True)
    (root / (key + '-artifacts.json')).write_text((isolated / (key + '-artifacts.json')).read_text())
