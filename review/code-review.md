# Code Review: repo-man initial implementation (main → repoman-init)

Scope: full diff, 52 files / ~9,900 lines (cmd/repoman, internal/*, Dockerfile,
compose.yaml, CI workflow, docs). Everything below is new code — no prior
version exists to regress against, so "removed behavior" findings are read as
"an invariant the code clearly intends but doesn't fully enforce."

Overall: this is an unusually careful, well-documented implementation —
extensive threat-model comments, thorough tests (including race-detector and
symlink-escape tests), and consistent defense-in-depth around path handling
and subprocess execution. The findings below are the real gaps that survived
a high-recall pass; most are edge cases rather than everyday breakage.

---

## High severity

### 1. Job goroutine has no panic recovery — one bad clone can take down the whole service
**File:** `internal/jobs/jobs.go:136-155` (`Registry.Start`)

`Start` launches `fn` (in practice `git.Clone`, see `internal/actions/actions.go:118`)
in a bare goroutine:

```go
go func() {
    defer r.wg.Done()
    err := fn(r.base, &TailWriter{buf: e.buf})
    ...
}()
```

There is no `recover()` anywhere in the codebase (`grep -rn "recover()" internal/
cmd/` returns nothing). Every other resource-lifecycle concern in this file is
carefully guarded (eviction caps, TTL gc, output-tail bounding), but a panic
inside `fn` is not caught. Since this is a single-process, single-tenant
service, a panic here doesn't just fail the one job — it crashes the entire
`repoman` process, taking down the HTTP server, every other in-flight job, and
all live sessions.

**Failure scenario:** any unexpected input that trips a nil dereference or
index-out-of-range somewhere in the git-output-parsing path (most likely via a
future change, but also plausible today against unusual git output) turns a
single clone into a full outage instead of one job marked `StateFailed`.

**Suggested fix:** wrap the job body in the goroutine with a `defer recover()`
that records the panic as a failed job (`e.job.State = StateFailed; e.job.Err =
fmt.Sprintf("panic: %v", r)`), mirroring how a normal `error` return is
already handled.

---

### 2. A failed clone leaves an orphaned directory that permanently blocks retrying
**File:** `internal/git/git.go:171-203` (`Clone`) and `internal/actions/actions.go:109-113` (`Clone`)

`git.Clone` creates `absPath` as a side effect of running `git clone` and does
not remove it if the clone subprocess fails partway through (network drop,
revoked credentials, interrupted host, disk full):

```go
cmd := exec.CommandContext(ctx, "git", "-c", "protocol.ext.allow=never", "clone", "--progress", "--", url, absPath)
...
if err := cmd.Run(); err != nil {
    return gitError([]string{"clone", url, absPath}, stderr.Bytes(), err)
}
```

`actions.Service.Clone`'s pre-flight check treats *any* existing entry at the
destination as a conflict, with no way to distinguish "a real checkout" from
"wreckage from a previous failed attempt":

```go
if _, err := os.Lstat(abs); err == nil {
    return jobs.Job{}, fmt.Errorf("%w: %s", ErrExists, rel)
}
```

**Failure scenario:** a clone job fails midway (a very ordinary occurrence —
flaky network, expired credentials, a large repo hitting `FetchTimeout`-style
issues). The job registry correctly marks it `StateFailed`, but the partially
populated directory stays on disk. Any later retry of the same clone — same
derived or explicit destination — is rejected with 409/`ErrExists` before it
ever reaches git again. The operator has no UI-driven recovery path and must
manually delete the directory outside the dashboard, which defeats the point
of the tool for exactly the case (a clone that didn't go smoothly) where it
matters most.

**Suggested fix:** on a `Clone` failure, remove `absPath` if it was created by
this invocation (e.g. `os.RemoveAll` guarded by a "did we create it" flag, or
simply `defer` a cleanup that only fires when `cmd.Run()` errors and the
directory is otherwise empty/partial). Alternatively, have `actions.Service`
distinguish a directory containing a real checkout from empty/partial wreckage
before returning `ErrExists`.

---

### 3. Login body-size limit is only enforced on the JSON path, not the form path
**File:** `internal/auth/auth.go:316-336` (`readLoginRequest`)

The package doc and the `maxLoginBodyBytes` comment both assert this package
"doesn't assume an outer handler has already applied a body-size limit." The
JSON branch honors that:

```go
dec := json.NewDecoder(io.LimitReader(r.Body, maxLoginBodyBytes))
```

but the form-encoded branch — which is what the actual `<form method="post"
action="/login">` in `login.html` submits — calls `r.ParseForm()` directly
with no `http.MaxBytesReader`/`io.LimitReader` wrapping `r.Body` first:

```go
if err := r.ParseForm(); err != nil {
    return "", "", err
}
return r.FormValue("passphrase"), r.FormValue("next"), nil
```

Go's stdlib `ParseForm` falls back to its own default cap (10MB) when `Body`
isn't already a size-limited reader — 160x the documented 64KB.

**Failure scenario:** an unauthenticated client repeatedly POSTs `/login`
(`Content-Type: application/x-www-form-urlencoded`) with bodies up to ~10MB.
Every such request is read fully into memory before the passphrase is even
compared, on an endpoint that by definition takes anonymous traffic. This is
a real, if modest, resource-exhaustion vector that the code's own comment says
should not exist.

**Suggested fix:** wrap the form branch the same way the JSON branch is
wrapped: `r.Body = http.MaxBytesReader(nil, r.Body, maxLoginBodyBytes)` (or an
`io.LimitReader`) before calling `r.ParseForm()`.

---

## Medium severity

### 4. `ValidateCloneURL` doesn't catch a dash-prefixed host in scp-like form
**File:** `internal/git/remote.go:79-126` (`ValidateCloneURL`), regex at `remote.go:17`

The leading-dash guard only checks the start of the *whole* string:

```go
if strings.HasPrefix(trimmed, "-") {
    return "", errors.New("clone URL must not start with a dash")
}
...
case scpLikeRe.MatchString(trimmed):
    return trimmed, nil
```

`scpLikeRe` is `^(?:[^@/\s]+@)?([A-Za-z0-9.-]+):(.+)$` — the host character
class itself includes `-`. A value like `user@-oProxyCommand=...:path`
doesn't start with `-` (it starts with `user@`), so the guard passes, and the
regex happily captures `-oProxyCommand=...` as the host:

```
>>> re.match(r'^(?:[^@/\s]+@)?([A-Za-z0-9.-]+):(.+)$', 'user@-oProxyCommand:path').group(1)
'-oProxyCommand'
```
(verified directly). This is handed to `git clone -- <url> <dest>` unmodified.
Modern git versions independently reject a dash-prefixed ssh host at their own
layer (post CVE-2017-1000117 hardening), so this is likely mitigated in
practice today — but the function's own doc comment claims it "rejects
anything ... that could be used to smuggle a git option or invoke a dangerous
transport," and for this input shape that guarantee doesn't actually hold.

**Suggested fix:** after a scp-like match, additionally reject when the
captured host group (`m[1]`) itself starts with `-`, not just when the whole
trimmed string does.

---

### 5. A dot-prefixed name/destination passes validation but is permanently invisible to the scanner
**File:** `internal/safepath/safepath.go:124-145` (`ValidName`) vs. `internal/scan/scan.go:83-90` (`walkChildren`)

`scan.Walk` unconditionally skips any dot-prefixed directory:

```go
if strings.HasPrefix(name, ".") {
    // A dot-directory (.cache, .config, and so on) is never a
    // checkout the user wants listed...
    continue
}
```

but `safepath.ValidName` — the only gate `actions.InitRepo`'s `name` parameter
and `actions.DestFor`'s per-segment shorthand-derived path go through — only
rejects a segment that is *exactly* `.` or `..`, not merely dot-*prefixed*:

```go
case s == "." || s == "..":
    return fmt.Errorf("name %q is reserved", s)
```

**Failure scenario:** `POST /api/init` with `name=".hidden"`, or `POST
/api/clone` with an explicit `dest` containing a `.`-prefixed segment, passes
`safepath.Resolve`/`ValidName`, `git init`/`git clone` succeeds, and the
handler returns 201/202 with a real path. Every subsequent `scan.Walk` skips
that directory, so the repository never appears in any snapshot, tree
fragment, or `/api/tree` response — a fully functional checkout that is
permanently invisible in the dashboard, with no error ever surfaced to the
user to explain why.

**Suggested fix:** either have `safepath.ValidName` reject any segment
starting with `.` (simplest, and consistent with the scanner's own rule), or
have `scan.Walk` distinguish "hidden support directory" from "a checkout the
user explicitly created here" some other way.

---

### 6. Worktree-directory pruning re-derives "is this a worktree holder" from the name it says elsewhere is unreliable
**File:** `internal/tree/tree.go:172-187` (`pruneEmptyWorktreeDirs`)

```go
if c.Kind == model.KindDir && len(c.Children) == 0 && strings.HasSuffix(c.Name, "-worktrees") {
    continue // dropped from the tree
}
```

This directly contradicts the principle stated by the scanner itself
(`internal/scan/scan.go:193-197`) and the README ("Worktrees are detected by
their `.git` *file* and its `gitdir:` pointer, not by the `-worktrees`
directory name"): the `-worktrees` suffix is documented as *not* a reliable
signal, yet the pruning step uses exactly that suffix to decide what to
delete from the displayed tree.

**Failure scenario:** a user creates (or is left with) an ordinary empty
directory that happens to match the pattern, e.g. `foo-worktrees` created via
"New repo" as a staging spot before deciding what to put in it — the same
naming convention repo-man itself produces. It gets silently dropped from the
tree on the very next scan, contradicting the neighboring code's own stated
policy that other empty directories are kept because "the user created those
deliberately."

**Suggested fix:** track which directory nodes were actually created to hold
worktrees during the re-parenting walk in `Build` (around `tree.go:76-94`),
and prune only those — not directories that merely match a naming pattern.

---

### 7. Ghost/repo dedup breaks on host case, causing a cloned repo to duplicate as a ghost forever
**File:** `internal/tree/tree.go:100-108` (dedup by `Slug()`) vs. `internal/config/config.go:185-216` (`ParseOwners`) and `internal/git/remote.go:53` (`ParseRemoteURL`)

A locally scanned remote's host is lowercased by `ParseRemoteURL`:

```go
host = strings.ToLower(host)
```

but a configured owner's host from `-owners`/`REPOMAN_OWNERS` is never
normalized by `ParseOwners`, and neither is the host `internal/github`
attaches to a `Ghost` it builds from that owner. Dedup compares the two
slugs directly:

```go
cloned := make(map[string]bool, len(in.Repos))
for _, r := range in.Repos {
    if s := r.Remote.Slug(); s != "" {
        cloned[s] = true
    }
}
...
if cloned[g.Slug()] { continue }
```

**Failure scenario:** an operator configures `-owners "GitHub.com/jbain"`
(a plausible typo, or just habit). Every ghost for that owner gets
`Host="GitHub.com"`. Cloning one via the UI posts the ghost's own `Rel` as an
explicit `dest`, so `actions.Service.Clone` skips `DestFor` (which would have
derived a lowercase path) and the repo lands on disk under
`GitHub.com/jbain/...`. The next scan reads the new checkout's origin URL
through `ParseRemoteURL`, which *does* lowercase the host, producing
`Remote.Slug() == "github.com/jbain/repo"` — which never matches
`Ghost.Slug() == "GitHub.com/jbain/repo"`. The repo now permanently shows up
twice: once as a real checkout, once as a ghost claiming to be uncloned.

**Suggested fix:** normalize host casing once, in `ParseOwners` (and/or in
`model.Owner.Rel()`/`Ghost.Slug()`), so every producer of a slug agrees on
case the same way `ParseRemoteURL` already does.

---

### 8. `InitRepo` has no concurrency guard, unlike `Clone`
**File:** `internal/actions/actions.go:52-77` (`InitRepo`)

`Clone` is protected against a duplicate concurrent request for the same
target via `jobs.Registry.Start`'s busy-check (`ErrBusy`, see
`internal/jobs/jobs.go:110-120`). `InitRepo` runs synchronously and is never
registered with the job registry, so nothing serializes two `InitRepo` calls
for the same `parent`+`name`:

```go
if _, err := os.Lstat(abs); err == nil {
    return "", fmt.Errorf("%w: %s", ErrExists, path.Join(parentClean, rel))
} else if !errors.Is(err, os.ErrNotExist) {
    return "", err
}
if err := git.Init(ctx, abs, branch); err != nil { ... }
```

**Failure scenario:** two near-simultaneous `POST /api/init` requests for the
same `parent`+`name` (a double-click, or two browser tabs) both pass the
`os.Lstat` "not exists" check before either has created the directory, and
both then call `git.Init` concurrently against the same path — racing
`MkdirAll` and `git init -b <branch>` against one directory, instead of the
second request cleanly getting `ErrExists` as the design intends. (The
existing test, `TestInitRepoCreatesARepository`, only exercises this
sequentially, so it doesn't catch the race.)

**Suggested fix:** route `InitRepo` through the same registry-based
mutual-exclusion `Clone` already gets, or add a lightweight in-process lock
keyed by the resolved destination path.

---

## Low severity

### 9. `POST /logout` is itself behind the auth gate, so it never runs for an already-invalid session
**File:** `internal/web/server.go:72-92` (`Handler`)

```go
mux.HandleFunc("POST /logout", s.auth.Logout)
...
protected := s.auth.Require(mux)
```

`/logout` is registered on `mux`, which is then wrapped by `s.auth.Require`.
`auth.Logout`'s entire job is to delete the server-side session and send an
expired cookie so the browser drops it — but if the request's cookie is
already invalid, expired, or absent (e.g. after a process restart, which the
package doc explicitly calls out as an expected event that logs everyone
out), `Require` denies the request before `Logout` ever runs, so the
already-stale cookie is never explicitly cleared client-side.

**Failure scenario:** after a restart or natural expiry, a browser holding
the old `repoman_session` cookie clicks "Sign out" and is redirected to
`/login` instead of getting a clean "signed out" response; the browser keeps
sending the (harmless but never-cleared) cookie on every future request.

**Suggested fix:** register `/logout` on the public mux (unauthenticated),
matching the pattern used for `/login` — `Logout` already tolerates a
missing/invalid cookie gracefully (`if c, err := r.Cookie(...); err == nil &&
c.Value != ""`).

---

### 10. `clientIP` only looks at the first `X-Forwarded-For` header line
**File:** `internal/auth/ip.go:27-41` (`clientIP`)

```go
if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
    parts := strings.Split(xff, ",")
    if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
        return last
    }
}
```

The comment above this function carefully reasons about taking the *last*
comma-separated hop *within one header line* — correct for the common case
where a proxy appends to the existing header value. But `http.Header.Get`
returns only the first occurrence when a request carries `X-Forwarded-For` as
multiple separate header lines (Go's `Header.Get` docs: "gets the first value
associated with the given key"). Depending on the specific reverse-proxy
configuration in front of the service, a proxy that adds a new header line
rather than merging into the client's existing one would mean `Get()` returns
the attacker-supplied value instead of the trusted one.

**Failure scenario:** with `TrustProxyIP` enabled, a client sends the request
with its own `X-Forwarded-For: 1.2.3.4` already set; if the proxy in front
appends its own line rather than rewriting/merging the header, `clientIP`
returns the attacker's `1.2.3.4` instead of the proxy-observed address,
letting the attacker rotate the claimed IP to dodge login-rate-limit lockout.

**Suggested fix:** use `r.Header.Values("X-Forwarded-For")` to collect every
header line, join them, and take the last comma-separated entry of the
combined list — or explicitly document that the reverse proxy must be
configured to merge rather than append `X-Forwarded-For` lines.

---

## Notable cleanup opportunities (not included above due to the finding cap)

These didn't make the top 10 but are worth a look in a follow-up pass:

- `internal/git/status.go`'s `Inspect` makes five sequential git subprocess
  spawns per checkout (`status.go:27,42,50,51,54`); the independent ones
  (stash count, origin URL, common-dir) could run concurrently, and this cost
  is multiplied by `inspectConcurrency` (8) parallel checkouts every scan.
- `internal/index/index.go:327`'s `refreshGhosts` calls `ListOwner` for each
  configured owner sequentially rather than concurrently, unlike `scan()`/
  `fetchAll()` in the same file which already use a semaphore+`WaitGroup` for
  this exact kind of per-target fan-out.
- Several small helpers are duplicated rather than shared: `writeJSON` (both
  `internal/auth/auth.go:376` and `internal/web/server.go:346`), the
  "exec + hardened env + truncated stderr error" pattern (`internal/git/git.go:25`
  vs `internal/github/github.go:121-177`), `validateLogin`/`validateHost`
  (`internal/github/parse.go:96,113`), and the "leading dash rejected because
  it becomes a CLI flag" rule (reimplemented independently in at least seven
  places across `safepath`, `git`, and `github`).
- `internal/web/funcs.go`'s `badges()` and `statusWord()` rank status
  conditions in different orders (behind/ahead before dirty in one, dirty
  before behind/ahead in the other), so the row's accent color can disagree
  with which badge is visually first.
