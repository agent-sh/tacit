"""Validate an external Laya source checkout before importing it."""
import argparse
from pathlib import Path
import subprocess
import sys

PIN = '4ac226d5ee88ed6df8db288a05c766d7a34ee682'


def check_source(source):
    source = Path(source).expanduser().resolve()
    commit = subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip()
    if commit != PIN:
        raise ValueError('Laya source must be checked out at ' + PIN)
    if subprocess.check_output(['git', '-C', str(source), 'status', '--porcelain', '--untracked-files=all'], text=True).strip():
        raise ValueError('Laya source checkout must be clean, including untracked files')
    if not (source / 'laya/__init__.py').is_file():
        raise ValueError('expected Laya source package')
    sys.path.insert(0, str(source))
    return source


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source', required=True)
    a = p.parse_args()
    print(check_source(a.source))


if __name__ == '__main__':
    main()
