# Playback Transcode and Failover Hardening Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the `5168x2160` VAAPI hardware encoder failure, prevent width/height upscaling across all transcode pipelines, clamp target resolutions to effective source resolution, and harden virtual playback fallback to iteratively attempt compatible alternate versions on startup failure.

**Architecture:**
- **Scaling Policy:** Centralize safe filter construction for CPU, QSV, VAAPI, NVENC, HDR tone-mapping, and subtitle burn-in. Cap output height to source height (`h=min(ih\,target)`) and cap output width to hardware encoder limits (`w=min(iw\,4096)` / `force_original_aspect_ratio=decrease`).
- **Handler Target Clamping:** Add `clampEncodedTargetResolution` in `internal/api/handlers/playback.go` so encoded transcode targets never exceed effective source resolution.
- **Iterative Virtual Fallback:** Update `playback_v3.go` so that runtime `transcode_start_failed` for virtual files can iterate through available non-4K/SDR alternate candidates, even when initial request specified `"original"` quality preference.

---

## Task List

### Task 1: Create Plan File and Bounded Scaling Policy in `internal/playback`

- [x] **Step 1: Write bounded scale filter helpers in `internal/playback/transcode.go`**
  - Ensure `vaapiScaleFilter`, `qsvScaleFilter`, `nvencScaleFilter`, and `resolutionToScale` cap height to source height (`h=min(ih\,<target>)`) and restrict maximum generated width to encoder constraints (e.g. 4096 for H.264 VAAPI).
  - Ensure HDR tone mapping (`hdrToSDRFilter`) and subtitle burn-in filters (`appendBitmapSubtitleBurnInArgs`, `appendSubtitleBurnInArgs`) use the safe scaling expressions.

- [x] **Step 2: Add comprehensive scaling tests in `internal/playback/transcode_args_test.go`**
  - Verify CPU, QSV, VAAPI, and NVENC filter strings.
  - Verify that 4K target resolution on 1080p or 1608p ultrawide input does not upscale height or exceed max width.

### Task 2: Implement Handler Target Resolution Clamping

- [x] **Step 1: Add `clampEncodedTargetResolution` in `internal/api/handlers/playback.go`**
  - Clamp requested resolution to source resolution for encoded video targets.
  - Add unit tests `TestClampEncodedTargetResolution` in `internal/api/handlers/playback_test.go`.

- [x] **Step 2: Wire target resolution clamping into playback planning**
  - Apply target resolution clamping after alternate file selection before local/remote transport planning.

### Task 3: Harden Virtual Playback Startup Fallback

- [x] **Step 1: Multi-candidate alternate resolution in `internal/api/handlers/playback.go` / `playback_v3.go`**
  - Add `findAlternateFiles` returning ordered alternate candidates for a media item.
  - Update `startPlannedPlaybackV3` in `playback_v3.go` so `transcode_start_failed` on virtual media loops through candidates until one succeeds or all fail.
  - Ensure `isVirtualPlaybackFile(requestedFile)` guard protects ordinary non-virtual files from unexpected version switching.

- [x] **Step 2: Add end-to-end failover tests in `internal/api/handlers/playback_v3_test.go`**
  - Test virtual file with `"original"` quality fallback to alternate on `transcode_start_failed`.
  - Test multi-candidate fallback where candidate 1 fails and candidate 2 succeeds.
  - Test non-virtual file with `"original"` quality does not switch files.

### Task 4: Full Verification, Docker Rebuild, and Container Deployment

- [ ] **Step 1: Run complete Go test suite & lint**
  - `go test ./...`
  - `golangci-lint run ./internal/playback/... ./internal/api/handlers/...`

- [ ] **Step 2: Build Docker image & deploy to compose container**
  - `docker build -t silo-server:latest .`
  - `docker compose up -d --no-deps --force-recreate silo` in `/opt/silo`
  - Verify `/api/v1/ready` returns `{"status":"ok"}`.
