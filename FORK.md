# Fork: video transcode on upload

This is a small fork of [GoToSocial](https://codeberg.org/superseriousbusiness/gotosocial)
(pinned to the tag this branch was cut from) that adds one thing upstream does
not do: **downscaling oversized video uploads before they are stored.**

Upstream stores media byte-for-byte and only generates a thumbnail. That means a
phone-recorded 1080p clip at ~16 Mbps lands as ~126 MB for one minute, and every
remote instance that fetches it transfers all of it. Mastodon transcodes
server-side; GoToSocial deliberately does not.

## What it does

On upload, a video wider than `VideoTranscodeMaxWidth` (640 px) is re-encoded
before being written to storage:

- scaled to 640 px wide, aspect ratio kept, height rounded to an even number
- H.264, CRF 26, `veryfast`
- audio re-encoded to AAC 96 kbps
- MP4 with `+faststart`, so playback starts before the whole file is fetched
- the stored attachment metadata (width, height, aspect, bitrate, duration, file
  size) is re-probed from the transcoded file, so the API reports the truth

A one-minute 1080p clip goes from ~126 MB to ~4 MB. Anything already within the
limit is left untouched (`ErrTranscodeNotNeeded`), and nothing is ever upscaled.

If transcoding fails for any reason the upload still succeeds: the original is
stored after metadata cleaning, and a warning is logged. Losing a post is worse
than keeping a large file.

## Where

| File | Role |
|---|---|
| `internal/media/transcode.go` | `shouldTranscodeVideo`, `transcodeVideo`, output verification |
| `internal/media/transcode_test.go` | unit tests (real ffmpeg, real ffprobe) |
| `internal/media/processingmedia.go` | the `FileTypeVideo` branch of the upload pipeline |

## Knobs

| Constant | Default | Meaning |
|---|---|---|
| `VideoTranscodeMaxWidth` | `640` | width above which uploads are downscaled |
| `videoTranscodeCRF` | `"26"` | x264 quality target |
| `videoTranscodePreset` | `"veryfast"` | speed/size tradeoff |
| `videoTranscodeAudioBitrate` | `"96k"` | AAC audio bitrate |

The ffmpeg binary is looked up on `PATH` by default. It can be pinned at build
time, which is what the NixOS package does:

```
-ldflags "-X code.superseriousbusiness.org/gotosocial/internal/media.transcodeFfmpegPath=/path/to/ffmpeg"
```

## Notes for maintainers

- The bundled (WASM) ffprobe only grants access to files it was told about, by
  basename, with `os.TempDir()` as the mount root. Tests therefore create
  fixtures directly in `os.TempDir()` rather than in `t.TempDir()` — a nested
  directory makes the probe fail with `EPERM`. Production writes media temp
  files flat in the same directory, so this matches reality.
- Only local uploads are affected. Remote media is still governed by
  `media-remote-max-size`.
- Existing attachments are not migrated; this only applies to new uploads.

AGPL-3.0-or-later, same as upstream.
