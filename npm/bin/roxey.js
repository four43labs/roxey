#!/usr/bin/env node
'use strict';
// Thin wrapper so `npx roxey ...` works with no separate install: downloads
// the prebuilt Go CLI binary for this platform from the matching GitHub
// Release (published by .github/workflows/release.yml), caches it under
// ~/.roxey/bin, and execs it. All the actual CLI logic lives in the Go
// binary — this file only knows how to fetch and run it.

const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const https = require('https');
const { execFileSync, spawnSync } = require('child_process');

const REPO = 'four43labs/roxey';

const PLATFORM_MAP = { darwin: 'darwin', linux: 'linux' };
const ARCH_MAP = { x64: 'amd64', arm64: 'arm64' };

function fail(msg) {
  console.error(`roxey: ${msg}`);
  process.exit(1);
}

const goos = PLATFORM_MAP[process.platform];
const goarch = ARCH_MAP[process.arch];
if (!goos || !goarch) {
  fail(`unsupported platform ${process.platform}/${process.arch} (Roxey ships for macOS and Linux, amd64 or arm64)`);
}

const { version } = require('../package.json');
const tag = `v${version}`;
const cacheDir = path.join(os.homedir(), '.roxey', 'bin');
const binPath = path.join(cacheDir, `roxey-${tag}-${goos}-${goarch}`);

function download(url, destPath, redirectsLeft = 5) {
  return new Promise((resolve, reject) => {
    const file = fs.createWriteStream(destPath);
    const client = url.startsWith('http://') ? http : https; // GitHub URLs are always https; http only used in local tests
    client
      .get(url, { headers: { 'User-Agent': 'roxey-npx' } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          file.close();
          fs.unlink(destPath, () => {
            if (redirectsLeft <= 0) return reject(new Error('too many redirects'));
            resolve(download(res.headers.location, destPath, redirectsLeft - 1));
          });
          return;
        }
        if (res.statusCode !== 200) {
          file.close();
          fs.unlink(destPath, () => reject(new Error(`download failed: HTTP ${res.statusCode} for ${url}`)));
          return;
        }
        res.pipe(file);
        file.on('finish', () => file.close(() => resolve()));
      })
      .on('error', (err) => fs.unlink(destPath, () => reject(err)));
  });
}

async function ensureBinary() {
  if (fs.existsSync(binPath)) return;
  fs.mkdirSync(cacheDir, { recursive: true });

  const assetURL = `https://github.com/${REPO}/releases/download/${tag}/roxey_${goos}_${goarch}.tar.gz`;
  const tmpTar = path.join(cacheDir, `.download-${process.pid}.tar.gz`);
  process.stderr.write(`roxey: downloading ${tag} for ${goos}/${goarch}...\n`);

  try {
    await download(assetURL, tmpTar);
    execFileSync('tar', ['-xzf', tmpTar, '-C', cacheDir]);
    fs.renameSync(path.join(cacheDir, 'roxey'), binPath);
    fs.chmodSync(binPath, 0o755);
  } finally {
    fs.unlink(tmpTar, () => {});
  }
}

ensureBinary()
  .then(() => {
    const result = spawnSync(binPath, process.argv.slice(2), { stdio: 'inherit' });
    if (result.error) throw result.error;
    process.exit(result.status === null ? 1 : result.status);
  })
  .catch((err) => fail(err.message));
