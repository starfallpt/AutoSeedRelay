#!/usr/bin/env python3
"""Generate qBittorrent.conf with a pre-set PBKDF2 password hash.

This bypasses the random-temp-password mechanism that modern qBittorrent
(>= 4.6.1) uses when no WebUI password is configured.

Usage:
    python3 scripts/init_qb_config.py [password] [config_dir]

Defaults:
    password: CHANGE_ME
    config_dir: ./data/qb-config

Config paths (auto-detected):
    qbittorrentofficial/qbittorrent-nox  -> <config_dir>/qBittorrent.conf
    linuxserver/qbittorrent              -> <config_dir>/qBittorrent/qBittorrent.conf

The generated hash uses PBKDF2-HMAC-SHA512 with 100,000 iterations and
a 16-byte random salt -- matching qBittorrent's own password storage format
(WebUI\\Password_PBKDF2).

WARNING: This overwrites any existing qBittorrent.conf. Only run this for
fresh installations. On restart, qB preserves the password set here.
"""

import hashlib
import os
import base64
import sys


def generate_pbkdf2_hash(password: str) -> str:
    """Generate a qBittorrent-compatible PBKDF2 password hash.

    Format: @ByteArray(base64(salt[16] || hash[32]))
    Algorithm: PBKDF2-HMAC-SHA512, 100,000 iterations
    """
    salt = os.urandom(16)
    # qB 5.x: PBKDF2-HMAC-SHA512, 100000 iterations, 32-byte output
    # Key order: salt (16 bytes) + derived key (32 bytes) = 48 bytes total
    key = hashlib.pbkdf2_hmac(
        'sha512',
        password.encode('utf-8'),
        salt,
        100000,
        dklen=32,
    )
    combined = salt + key
    encoded = base64.b64encode(combined).decode('ascii')
    return f'@ByteArray({encoded})'


def find_config_path(config_dir: str) -> str:
    """Find the correct qBittorrent.conf path.

    Official image: <config_dir>/qBittorrent/config/qBittorrent.conf
    Linuxserver:    <config_dir>/qBittorrent/qBittorrent.conf
    """
    # Official qbittorrent-nox path (--profile=/config → /config/qBittorrent/qBittorrent.conf)
    official_path = os.path.join(config_dir, 'qBittorrent', 'qBittorrent.conf')
    # Linuxserver path
    lsio_path = os.path.join(config_dir, 'qBittorrent', 'qBittorrent.conf')
    if os.path.exists(lsio_path):
        return lsio_path
    return official_path


def write_qb_config(config_dir: str, password: str, force: bool = False) -> bool:
    hash_value = generate_pbkdf2_hash(password)
    paths = [
        os.path.join(config_dir, 'qBittorrent', 'qBittorrent.conf'),          # main
        os.path.join(config_dir, 'qBittorrent', 'config', 'qBittorrent.conf'), # bootstrap
    ]
    for p in paths:
        if os.path.exists(p) and not force:
            print(f'[init_qb_config] {p} exists, skipping.')
            continue
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, 'w', encoding='utf-8') as f:
            f.write('[Preferences]\n')
            f.write(f'WebUI\\Password_PBKDF2="{hash_value}"\n')
            f.write('[Meta]\n')
            f.write('MigrationVersion=8\n')
        print(f'[init_qb_config] Created {p}')
    print(f'[init_qb_config] WebUI username: admin')
    print(f'[init_qb_config] WebUI password: {password}')
    return True
    print(f'[init_qb_config] WebUI username: admin')
    print(f'[init_qb_config] WebUI password: {password}')
    return True


if __name__ == '__main__':
    password = sys.argv[1] if len(sys.argv) > 1 else 'CHANGE_ME'
    config_dir = sys.argv[2] if len(sys.argv) > 2 else os.path.join(
        os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
        'data', 'qb-config',
    )
    force = '--force' in sys.argv

    write_qb_config(config_dir, password, force)
