# Hyperfleet

[![Go version](https://img.shields.io/github/go-mod/go-version/alexisbchz/hyperfleet?logo=go)](go.mod) [![Firecracker](https://img.shields.io/github/v/release/firecracker-microvm/firecracker?label=Firecracker&logo=github)](https://firecracker-microvm.github.io/)

> An orchestrator running microVMs on demand. Built for AI agent sandboxes, CI engines, and more.

## Contents

- [Installation](#installation)
  - [Requirements](#requirements)
  - [Setup](#setup)
- [Commands](#commands)
  - [`serve(1)` — hyperfleet daemon](#serve1--hyperfleet-daemon)
  - [`fleet(1)` — hyperfleet CLI](#fleet1--hyperfleet-cli)
  - [Make targets](#make-targets)
- [REST API](#rest-api)
  - [Authentication](#authentication)
  - [Resource: Machine](#resource-machine)
  - [Endpoints](#endpoints)
  - [Errors](#errors)
  - [OpenAPI](#openapi)
- [SSH gateway](#ssh-gateway)
  - [Connection](#connection)
  - [Authentication](#authentication-1)
  - [Console multiplexing](#console-multiplexing)
  - [Host key](#host-key)
  - [Limitations](#limitations)
- [License](#license)

## Installation

### Requirements

Tested on **Ubuntu 24.04.4 LTS (Noble Numbat)**, kernel 6.17 / x86_64. Other
modern Linux distros should work but haven't been verified.

- Linux x86_64 or aarch64 (Firecracker is Linux-only)
- KVM enabled and `/dev/kvm` accessible
- `systemd`, `sudo`, `git`
- Go ≥ 1.26
- `dmsetup`, `losetup`, `mkfs.ext4` (devmapper thin pool)
- ~12 GB free disk for the default loopback thin pool

### Setup

```sh
git clone https://github.com/alexisbchz/hyperfleet.git
cd hyperfleet

make bootstrap      # install containerd + runc, devmapper thin pool, firecracker, vmlinux
make setup          # gum-driven: create containerd group, drop-in config, add $USER to it
newgrp containerd   # pick up the group in the current shell (or log out/in)
make run            # start the dev daemon on :8080 + ssh gateway on :2222
```

In another terminal, build and use the CLI:

```sh
make fleet                                          # builds ./bin/fleet
export FLEET_API_KEY=<key from `make run` output>   # or set it in ~/.zshrc
./bin/fleet machines create docker.io/library/alpine:3.20
./bin/fleet machines list
./bin/fleet machines ssh
```

`make bootstrap` is idempotent; re-runs are safe. `make setup` only needs to run
once per host.

## Commands

### `serve(1)` — hyperfleet daemon

**SYNOPSIS**

```
serve [--addr :8080] [--ssh-addr :2222] [--api-key KEY]
      [--containerd-sock PATH] [--namespace NAME] [--snapshotter NAME]
      [--firecracker-bin PATH] [--kernel-path PATH] [--work-root DIR]
```

**DESCRIPTION**

Runs the REST API (`/machines`) and SSH gateway. Each `POST /machines`
provisions a microVM asynchronously; SSH sessions attach to the VM's serial
console.

**OPTIONS**

| flag | env | default |
|---|---|---|
| `--addr` | `ADDR` | `:8080` |
| `--ssh-addr` | `SSH_ADDR` | `:2222` |
| `--api-key` | `API_KEY` | _ephemeral random_ |
| `--containerd-sock` | `CONTAINERD_SOCK` | `/run/containerd/containerd.sock` |
| `--namespace` | `CONTAINERD_NAMESPACE` | `hyperfleet` |
| `--snapshotter` | `SNAPSHOTTER` | `devmapper` |
| `--firecracker-bin` | `FIRECRACKER_BIN` | `./bin/firecracker` |
| `--kernel-path` | `KERNEL_PATH` | `./assets/vmlinux` |
| `--work-root` | `WORK_ROOT` | `./run` |

---

### `fleet(1)` — hyperfleet CLI

**SYNOPSIS**

```
fleet [--api-url URL] [--api-key KEY] [--ssh-host H] [--ssh-port P]
      [--output table|json] [--non-interactive] <command>
```

**COMMANDS**

```
fleet machines create [<image>]    create a machine; prompts for image if omitted
fleet machines list                list machines
fleet machines get [<id>]          show one machine; prompts for id if omitted
fleet machines delete [<id>]       delete a machine; prompts + confirm if id omitted
fleet machines ssh [<id>]          attach an interactive shell over the SSH gateway
```

**OPTIONS**

| flag | env | default |
|---|---|---|
| `--api-url` | `FLEET_API_URL` | `http://localhost:8080` |
| `--api-key` | `API_KEY`, `FLEET_API_KEY` | _required_ |
| `--ssh-host` | `FLEET_SSH_HOST` | _api-url host_ |
| `--ssh-port` | `FLEET_SSH_PORT` | `2222` |
| `--output, -o` | — | `table` |
| `--non-interactive` | `FLEET_NON_INTERACTIVE` | _auto: off when stdin is a TTY_ |

**EXAMPLES**

```sh
fleet machines create docker.io/library/alpine:3.20
fleet machines list -o json
fleet machines ssh                # interactive picker
```

---

### Make targets

```
make setup               # gum-driven host setup: containerd group + permissions
make install-containerd  # install containerd v2 + runc + systemd unit
make install-firecracker # install Firecracker binary into ./bin
make setup-devmapper     # create loopback thin pool + drop-in config
make kernel              # download vmlinux into ./assets
make bootstrap           # install-containerd + setup-devmapper + install-firecracker + kernel
make run                 # go run ./cmd/serve
make build               # build ./bin/serve and ./bin/fleet
make fleet               # build only ./bin/fleet
make tidy                # go mod tidy
```

## REST API

Base URL: `http://<host>:8080` (default). Content type: `application/json` for both
request and response bodies. Timestamps are RFC 3339 / ISO 8601 in UTC.

### Authentication

All endpoints under `/machines` require the API key on every request. Two
schemes are accepted (use either):

```
X-API-Key: <key>
Authorization: Bearer <key>
```

Missing or wrong key → `401 Unauthorized`. The OpenAPI document and Stoplight
docs UI (`/openapi.json`, `/openapi.yaml`, `/docs`) are intentionally public.

### Resource: Machine

```
{
  "id":        string,    // CUID, immutable, e.g. "ck5g9k1xa0000g0qja1xrjgqa"
  "image":     string,    // OCI image reference as supplied at create time
  "status":    string,    // one of: "pending" | "running" | "exited" | "failed"
  "createdAt": string,    // RFC 3339, set on POST /machines
  "startedAt": string,    // RFC 3339, present once status == "running"
  "exitedAt":  string,    // RFC 3339, present once status ∈ {"exited","failed"}
  "error":     string     // present iff status == "failed"; human-readable cause
}
```

State machine:

```
                   ┌──────────► running ──────────► exited
created (pending) ─┤
                   └──────────► failed
```

`pending → running` requires: lease acquisition, image pull, snapshot prepare,
work-dir creation, Firecracker start. Any step failing transitions directly to
`failed` with `error` set. `running → exited` is the normal termination path
(VMM exits cleanly). `running → failed` happens if `m.Wait` returns an error
that wasn't caused by host-initiated cancellation (i.e. `DELETE`).

### Endpoints

#### `POST /machines` — create

Provisions a machine **asynchronously** and returns immediately.

Request body:

```json
{ "image": "docker.io/library/alpine:3.20" }
```

`202 Accepted` response (representative):

```json
{
  "id":        "ck5g9k1xa0000g0qja1xrjgqa",
  "image":     "docker.io/library/alpine:3.20",
  "status":    "pending",
  "createdAt": "2026-05-04T09:10:31.997Z"
}
```

Status codes:

| code | meaning |
|---|---|
| `202` | accepted; provisioning runs in background |
| `400` | `image` missing or empty |
| `401` | bad / missing API key |

Idempotency: not supported — every call creates a new resource with a fresh CUID.

Notes:

- The HTTP response is sent **before** the image pull starts. Subsequent
  `GET /machines/{id}` reflects progress.
- Provisioning holds a containerd lease scoped to 15 minutes (`leases.WithExpiration`)
  so leaked snapshots are GC'd automatically if the daemon crashes.

#### `GET /machines` — list

Returns all known machines (in-memory; persistence is deferred).

`200 OK`:

```json
{ "machines": [ { /* Machine */ }, … ] }
```

No pagination, no filtering in v0. Order is unspecified.

#### `GET /machines/{id}` — get

| code | meaning |
|---|---|
| `200` | body is a `Machine` |
| `401` | bad / missing API key |
| `404` | unknown id |

#### `DELETE /machines/{id}` — delete

Cancels the per-machine context (Firecracker exits, snapshot is released,
work-dir is removed) and **blocks** until the lifecycle goroutine has fully
returned, then drops the record from the map.

| code | meaning |
|---|---|
| `204` | deleted; body is empty |
| `401` | bad / missing API key |
| `404` | unknown id |

Calling `DELETE` on an already-`exited` or `failed` machine returns `204` and
removes the record. A second `DELETE` on the same id returns `404`.

### Errors

Errors follow [RFC 7807 — Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc7807.html)
(Huma's default), served as `application/problem+json`:

```json
{
  "$schema": "http://<host>:8080/schemas/ErrorModel.json",
  "title":   "Not Found",
  "status":  404,
  "detail":  "machine not found"
}
```

Validation errors include an `errors` array enumerating each offending field.

The unauthenticated `401` is hand-written (not Problem Details) and returns:

```json
{ "error": "unauthorized" }
```

with `WWW-Authenticate: Bearer realm="hyperfleet"`.

### OpenAPI

| path | content |
|---|---|
| `/openapi.json` | OpenAPI 3.1 spec (JSON) |
| `/openapi.yaml` | OpenAPI 3.1 spec (YAML) |
| `/docs` | Stoplight Elements rendered docs |
| `/schemas/<Type>.json` | JSON Schemas referenced from `$schema` fields in responses |

Spec is generated at startup from the registered `huma.Operation`s in
`internal/api/api.go`; clients can be code-generated from it.

## SSH gateway

The daemon embeds an SSH server (built on
[`gliderlabs/ssh`](https://github.com/gliderlabs/ssh) wrapping
`golang.org/x/crypto/ssh`). It is **not** sshd-in-the-guest — there is no
networking inside the microVM and no per-image key management. Instead, an SSH
session on the host bridges the user's terminal to the VM's **serial console**
(ttyS0), which is wired through Firecracker's stdin/stdout.

### Connection

```
ssh -p 2222 <machine-id>@<host>
# or
fleet machines ssh <machine-id>
```

The daemon listens on `--ssh-addr` (default `:2222`). The address space the
SSH server exposes is "machines" — a session targeting username `<id>` is
routed to the corresponding `vmmgr.Manager.Attach(id)` call.

### Authentication

Password auth only, in v0:

| field | value |
|---|---|
| username | the machine `id` (CUID) |
| password | the API key (same value as the REST `API_KEY`) |

Comparison is constant-time (`crypto/subtle.ConstantTimeCompare`). Public-key
auth and per-user keys are deferred.

If the machine isn't `running` (e.g. still `pending` or already `exited`), the
session prints the reason and exits with code `1` instead of attaching.

### Console multiplexing

Each VM owns a `Console` (`internal/vmmgr/console.go`) that wraps the OS pipes
attached to Firecracker's stdin/stdout:

- **stdout (VM → user)**: a single goroutine reads from the Firecracker stdout
  pipe and broadcasts each chunk to every attached subscriber. Recent output is
  also kept in a 64 KiB ring buffer, so newly-attached subscribers see the
  most-recent activity (e.g., shell prompt) instead of a blank screen.
- **stdin (user → VM)**: writes from any subscriber go straight to the
  Firecracker stdin pipe. Multiple concurrent attachers share stdin
  (last-writer-wins, no arbitration).
- Slow subscribers drop chunks rather than blocking the broadcast (per-sub
  channel buffer = 64 chunks, non-blocking send).
- Closing the SSH session unsubscribes; the VM keeps running.
- Closing the VM closes every subscriber's read side with `io.EOF`.

The `fleet machines ssh` client requests a `xterm-256color` PTY when stdin is
a TTY and puts the local terminal into raw mode for the duration of the
session, so line discipline is performed by the guest kernel's tty driver, not
the host shell.

### Host key

A persistent ed25519 host key is generated on first start at:

```
${WORK_ROOT}/sshd_host_ed25519
```

(default: `./run/sshd_host_ed25519`, mode `0600`, PKCS#8 PEM). Subsequent
restarts reuse it, so clients' `known_hosts` entries stay valid. Delete the
file to rotate.

The bundled `fleet machines ssh` currently uses `ssh.InsecureIgnoreHostKey()`
— TOFU verification is on the v1+ list.

### Limitations

- **No `init=/bin/sh` ⇒ no real entrypoint**: until the initramfs+initd phase
  lands, every VM boots into a busybox shell on ttyS0 regardless of the OCI
  image's `Entrypoint`/`Cmd`.
- **No exec/signal RPCs**: SSH-into-serial is not the same as `docker exec`.
  The future vsock-based initd will expose typed `Exec`/`Signal`/`Wait` RPCs.
- **No per-machine SSH host keys**: the host key identifies the *gateway*,
  not the VM. Machines with the same id across daemon restarts present the
  same fingerprint.
- **Shared stdin** between concurrent sessions on the same VM (last-writer-wins).

## License

[AGPL-3.0](LICENSE)
