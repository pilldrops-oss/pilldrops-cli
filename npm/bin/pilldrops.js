#!/usr/bin/env node
// @pilldrops/cli — thin launcher for the pilldrops Go binary.
//
// The real CLI is a single static binary published on this repository's GitHub
// releases. postinstall fetches it next to this shim; if that was skipped
// (--ignore-scripts, offline install), the first run fetches it lazily. All
// args/stdio pass through. PILLDROPS_BASE_URL fetches from a server that serves
// the binaries itself under /dl/ instead.
const { existsSync, writeFileSync } = require('node:fs')
const { join } = require('node:path')
const { spawnSync } = require('node:child_process')

const RELEASES = 'https://github.com/pilldrops-oss/pilldrops-cli/releases/latest/download'
const EXE = process.platform === 'win32' ? '.exe' : ''
const BIN = join(__dirname, 'pilldrops-bin' + EXE)

async function ensureBinary() {
  if (existsSync(BIN)) return
  const os = { darwin: 'darwin', linux: 'linux', win32: 'windows' }[process.platform]
  const arch = { arm64: 'arm64', x64: 'amd64' }[process.arch]
  if (!os || !arch) {
    console.error(`@pilldrops/cli: unsupported platform ${process.platform}/${process.arch} (darwin/linux/win32, arm64/x64)`)
    process.exit(1)
  }
  const name = `pilldrops_${os}_${arch}${EXE}`
  const url = process.env.PILLDROPS_BASE_URL ? `${process.env.PILLDROPS_BASE_URL}/dl/${name}` : `${RELEASES}/${name}`
  console.error(`→ downloading pilldrops (${os}/${arch}) from ${url} …`)
  const res = await fetch(url)
  if (!res.ok) {
    console.error(`@pilldrops/cli: download failed (HTTP ${res.status}) from ${url}`)
    process.exit(1)
  }
  writeFileSync(BIN, Buffer.from(await res.arrayBuffer()), { mode: 0o755 })
  console.error('✓ pilldrops ready')
}

;(async () => {
  await ensureBinary()
  if (process.env.PILLDROPS_INSTALL_ONLY) return
  const r = spawnSync(BIN, process.argv.slice(2), { stdio: 'inherit' })
  process.exit(r.status ?? 1)
})()
