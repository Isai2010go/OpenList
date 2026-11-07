# Blomp storage driver

This driver integrates Blomp with OpenList through the OpenStack Swift API using the existing `github.com/ncw/swift/v2` dependency.

## Configuration

- **Username**: Blomp account email.
- **Password**: Blomp account password.
- **Auth URL**: Keystone v2 authentication URL. This field is intentionally required and has no default because the correct production URL has not yet been independently verified.
- **Tenant**: Swift tenant name. The default `storage` is a reported value, not a verified guarantee; confirm it with Blomp support.
- **Allow HTTP**: Off by default. Enable only if Blomp returns an HTTP object-store endpoint and you knowingly accept that the Swift token and file data can travel without transport encryption.

The driver authenticates with Keystone v2 and then verifies access to the object-store endpoint by listing containers. A successful identity authentication alone is not treated as proof that the storage proxy works.

## Layout

Swift containers are presented as top-level folders. Objects inside a container are shown as files and pseudo-directories. A trailing slash object is used when creating a directory.

## Current limitations / validation status

- Blomp's currently supported Keystone URL, tenant value, and returned object-store endpoint must be confirmed against a real Blomp account.
- Some public reports describe storage-proxy authorization failures (for example HTTP 403) after identity authentication. If this occurs, capture the redacted error and contact Blomp support; do not share account credentials.
- HTTP object-store endpoints are blocked unless **Allow HTTP** is explicitly enabled.
- This initial implementation has not yet been compiled against the full fork or tested with a live Blomp account. Treat it as development code, not a production-ready release.
- Directory rename/move/copy semantics depend on how Blomp implements Swift pseudo-directories and need live-account verification.

## Build / test

From the repository root:

```sh
gofmt -w drivers/blomp/*.go
go test ./drivers/blomp
go test ./drivers/...
```

Do not use a production Blomp password in CI logs or issue reports. Redact tokens, account identifiers, and signed URLs before sharing diagnostic output.
