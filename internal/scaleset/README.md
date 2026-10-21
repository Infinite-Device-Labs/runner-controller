# internal/scaleset

A patched copy of [actions/scaleset](https://github.com/actions/scaleset)
v0.4.0's root package, under its own MIT licence ([LICENSE](LICENSE)).

Only `internal/actions` imports it. Drop the copy, and import upstream again,
once upstream has the patch.

## What's upstream's

These files are v0.4.0's, byte for byte. Check them with `sha256sum -c` in
this directory:

```
3796c30c2888fd4aa41b3ca36bdfe3822aa3fc61985518409a814aae80c8793b  client.go
ab5b5e4e840b43687b457c1db090fe5158fbf7ffb57c8db6813f97d752f926c1  common_client.go
0523a5302fdcba40aa681fedc906a4f69f4d4ccfcf55127ff96d467637ca3173  config.go
d23fa365423e5685010f2559b6b1746d8033ed1043ff5e5adb955a76739078e4  errors.go
b29b8a8f6e287cc81d142029d9b3891f5d2547a0f812a76dd616f7870588d7f0  session_client.go
afdf51af352c8761af0e1b9e59c5ef00c00931776c12ae4b860fe4efad656639  types.go
2510b446bc1f0cf9702453075d20cd88631e20e5642658edb7325d9c1eb534f7  LICENSE
```

The `listener` and `examples` packages and the tests are left out.

## The patch

`patch_acquirable_jobs.go` adds `Client.GetAcquirableJobs`. It is
`GET {actions service}/_apis/runtime/runnerscalesets/{id}/acquirablejobs`,
which answers 200 with `{count, value}` or 204 when there are none.
actions-runner-controller's original listener called it when a session
opened. Without it, a job that became available while no session was open
waits for the scale set's next message (#6). The same file reports the
copy's version in the user agent.
