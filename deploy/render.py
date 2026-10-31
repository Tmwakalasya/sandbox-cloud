#!/usr/bin/env python3
"""Render deployment YAML with immutable image digests; never apply it."""
import argparse
import pathlib
import re

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--control-image', required=True)
parser.add_argument('--workspace-image', required=True)
args = parser.parse_args()
pattern = re.compile(r'[a-z0-9][a-z0-9./_-]+@sha256:[0-9a-f]{64}')
for value in (args.control_image, args.workspace_image):
    if not pattern.fullmatch(value):
        parser.error('image must be a registry path pinned with @sha256: and 64 lowercase hex characters')
source = pathlib.Path(__file__).with_name('api.yaml.template').read_text()
print(source.replace('CONTROL_IMAGE', args.control_image).replace('WORKSPACE_IMAGE', args.workspace_image), end='')
