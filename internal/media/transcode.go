// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"code.superseriousbusiness.org/gotosocial/internal/gtserror"
)

const (
	// VideoTranscodeMaxWidth is the maximum width, in pixels, of a locally
	// uploaded video. Anything wider is downscaled before being written to
	// storage. GoToSocial stores uploads byte-for-byte, so without this a
	// 16Mbps 1080p clip of one minute lands as ~126MB and every remote
	// instance that fetches it pays for the full thing.
	//
	// 640 is chosen for the fediverse reality: these are watched on phones in
	// a timeline, where 640px wide is indistinguishable from 1080p.
	VideoTranscodeMaxWidth = 640

	// videoTranscodeCRF is the x264 quality target. 26 at 640px is a large
	// size win with no visible loss on phone-sized playback.
	videoTranscodeCRF = "26"

	// videoTranscodePreset trades a little CPU for a much smaller file.
	videoTranscodePreset = "veryfast"

	// videoTranscodeAudioBitrate keeps audio transparent enough for speech.
	videoTranscodeAudioBitrate = "96k"
)

// ErrTranscodeNotNeeded is returned (wrapped nowhere) when the input is already
// within the limits, so callers can fall back to plain metadata cleaning.
var ErrTranscodeNotNeeded = errors.New("video already within transcode limits")

// transcodeFfmpegPath is the ffmpeg binary used for transcoding. It is a var so
// it can be pointed at an absolute store path at build time:
//
//	-ldflags "-X code.superseriousbusiness.org/gotosocial/internal/media.transcodeFfmpegPath=/nix/store/.../bin/ffmpeg"
var transcodeFfmpegPath = "ffmpeg"

// shouldTranscodeVideo reports whether a video with the given dimensions should
// be downscaled, and to what target width. Dimension-less media (audio files,
// broken probes) is never transcoded, and nothing is ever upscaled.
func shouldTranscodeVideo(width, height int) (int, bool) {
	if width <= 0 || height <= 0 {
		return 0, false
	}

	if width <= VideoTranscodeMaxWidth {
		return 0, false
	}

	return VideoTranscodeMaxWidth, true
}

// transcodeVideo downscales the video at inpath to targetWidth pixels wide,
// keeping the aspect ratio (and an even height, as yuv420p requires), and
// writes the result to a new file which it returns. Audio is re-encoded to AAC
// and the container is mp4 with a faststart index, so browsers and phones can
// start playing before the file is fully fetched.
//
// The returned path is always a new file; the caller owns both files and is
// responsible for removing the one it does not keep. If the input is already
// within the limit, ErrTranscodeNotNeeded is returned and no file is created.
func transcodeVideo(ctx context.Context, inpath string, targetWidth int) (string, error) {
	if targetWidth <= 0 {
		return "", gtserror.Newf("invalid target width %d", targetWidth)
	}

	// Probe the input first: the callers already have dimensions, but this
	// keeps the function safe to use on its own, and gives us the duration we
	// verify the output against.
	res, err := probe(ctx, inpath)
	if err != nil {
		return "", gtserror.Newf("error probing %s: %w", filepath.Base(inpath), err)
	}

	width, height, _ := res.ImageMeta()
	if width <= 0 || height <= 0 {
		return "", gtserror.New("input has no video stream")
	}

	if width <= targetWidth {
		return "", ErrTranscodeNotNeeded
	}

	// Write next to the input, with an mp4 extension so ffmpeg can infer the
	// muxer, then hand the path back.
	base := strings.TrimSuffix(inpath, filepath.Ext(inpath))
	outpath := base + "_transcoded.mp4"

	args := []string{
		// Only log errors.
		"-loglevel", "error",

		// Overwrite the (fresh) output path.
		"-y",

		// Input file path.
		"-i", inpath,

		// First video stream, plus all audio if present.
		"-map", "0:v:0",
		"-map", "0:a?",

		// Downscale to target width, keep aspect, round the height to an even
		// number (libx264 + yuv420p require it).
		"-vf", "scale=" + strconv.Itoa(targetWidth) + ":-2",

		// Transcode video.
		"-c:v", "libx264",
		"-preset", videoTranscodePreset,
		"-crf", videoTranscodeCRF,
		"-pix_fmt", "yuv420p",

		// Transcode audio, if any.
		"-c:a", "aac",
		"-b:a", videoTranscodeAudioBitrate,

		// Put the moov atom at the front for progressive playback.
		"-movflags", "+faststart",

		// Output.
		outpath,
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, transcodeFfmpegPath, args...)
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Never leave a partial file behind.
		_ = os.Remove(outpath)
		return "", gtserror.Newf("ffmpeg transcode failed: %w: %s", err, stderr.String())
	}

	// Verify the output before letting the caller store it: a truncated or
	// mis-scaled file published to the fediverse is worse than no transcode.
	if err := verifyTranscode(ctx, outpath, targetWidth, res.duration); err != nil {
		_ = os.Remove(outpath)
		return "", err
	}

	return outpath, nil
}

// verifyTranscode checks that the transcoded file is a real, playable video that
// respects the width limit and kept the input's duration.
func verifyTranscode(ctx context.Context, outpath string, targetWidth int, wantDuration float64) error {
	res, err := probe(ctx, outpath)
	if err != nil {
		return gtserror.Newf("error probing transcoded output: %w", err)
	}

	width, height, _ := res.ImageMeta()
	if width <= 0 || height <= 0 {
		return gtserror.New("transcoded output has no video stream")
	}

	if width > targetWidth {
		return gtserror.Newf("transcoded width %d exceeds target %d", width, targetWidth)
	}

	// Allow a second of slack: encoders re-time slightly, and rounding of the
	// frame count moves the last frame around.
	if wantDuration > 0 {
		diff := res.duration - wantDuration
		if diff < 0 {
			diff = -diff
		}
		if diff > 1.0 {
			return gtserror.Newf(
				"transcoded duration %.2fs differs from input %.2fs",
				res.duration, wantDuration,
			)
		}
	}

	return nil
}
