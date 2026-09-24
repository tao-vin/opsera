# Opsera Upload Stability Improvements

## Problem Summary

Current release workflows need a reliable way to upload large artifacts, wait for completion, verify integrity, and then promote the artifact atomically.

Observed issues:

- `file upload` can return success even when a large file is only partially transferred.
- `file upload-large` can return before the remote file is fully materialized.
- The caller has no reliable completion signal for `upload-large`.
- The caller must poll remote `.parts` directories manually.
- There is no first-class atomic promotion workflow for release artifacts.

## Impact

This creates slow and fragile release flows for large jars and APKs:

- The caller cannot trust a zero exit code.
- Release scripts need custom retry and polling logic.
- Partial uploads can be mistaken for completed releases.
- Operational time increases because humans must inspect remote state manually.

## Recommended Changes

### 1. Make `file upload` size-safe

If the target file is not fully transferred, the command must fail with a non-zero exit code.

Minimum behavior:

- Verify remote size after upload.
- Preferably verify remote SHA256 too.
- If the remote file is missing or smaller than the local file, fail.

### 2. Make `file upload-large` blocking

`upload-large` should not return until all of the following are complete:

- all chunks uploaded
- remote merge completed
- remote SHA256 computed
- final status known

The command should not return early while `.parts` is still being written.

### 3. Add progress output

Expose progress in a stable machine-readable form:

- chunk index / total chunks
- uploaded MB / total MB
- upload speed
- ETA
- merge state
- checksum state

### 4. Add atomic release upload

Introduce a command such as:

```powershell
opsera file upload-atomic --sso <local> <remote> --sha256 <hash>
```

Suggested behavior:

- upload to `<remote>.new`
- verify SHA256
- atomically `mv` to final path
- clean up temp parts on success/failure

### 5. Add a wait command

If background transfer is kept internally, add a wait command:

```powershell
opsera file wait --sso <remote> --sha256 <hash> --timeout 1800
```

This lets release scripts block until the artifact is actually ready.

### 6. Standardize command output

Every file-transfer command should return stable JSON:

```json
{
  "status": "done",
  "remote": "/data/application/cpcn/app.jar",
  "size": 369374158,
  "sha256": "...",
  "error": ""
}
```

## Acceptance Criteria

- A successful upload means the remote final file exists and matches size/hash.
- A failed upload must return non-zero.
- Large-file transfer must not silently continue after the CLI exits.
- Release scripts should not need ad hoc polling of `.parts` directories.

## Priority

1. Fix completion semantics for `upload` and `upload-large`
2. Add blocking verification
3. Add atomic upload workflow
4. Add structured progress/status output

