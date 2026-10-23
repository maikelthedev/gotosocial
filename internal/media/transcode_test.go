// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireFfmpeg skips the test when no ffmpeg binary is available.
func requireFfmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available in PATH")
	}
}

// transcodePath is the path transcodeVideo writes for a given input.
func transcodePath(inpath string) string {
	return strings.TrimSuffix(inpath, filepath.Ext(inpath)) + "_transcoded.mp4"
}

// tmpMediaPath returns a fresh path directly inside os.TempDir(). This is not
// cosmetic: the bundled WASM ffprobe only permits access to files whose
// basename it was told about, mounted at os.TempDir() as root, so a nested
// t.TempDir() would fail with EPERM. Production writes media files flat into
// the same directory, so tests must too.
func tmpMediaPath(t *testing.T) string {
	t.Helper()

	f, err := os.CreateTemp(os.TempDir(), "gts-transcode-*.mp4")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	p := f.Name()
	t.Cleanup(func() {
		_ = os.Remove(p)
		_ = os.Remove(transcodePath(p))
	})

	return p
}

// videoInfo is what a test needs to know about a media file.
type videoInfo struct {
	width    int
	height   int
	dur      float64
	vcodec   string
	hasAudio bool
}

// probeVideo probes the file at path into a videoInfo.
func probeVideo(t *testing.T, path string) videoInfo {
	t.Helper()

	res, err := probe(context.Background(), path)
	require.NoError(t, err)

	var info videoInfo
	info.width, info.height, _ = res.ImageMeta()
	info.dur = res.duration
	info.hasAudio = len(res.audio) > 0
	if len(res.video) > 0 {
		info.vcodec = res.video[0].stream.codec
	}

	return info
}

// makeTestVideo writes a synthetic video with audio at outpath.
func makeTestVideo(t *testing.T, outpath string, w, h int, dur float64) {
	t.Helper()

	size := strconv.Itoa(w) + "x" + strconv.Itoa(h)
	secs := strconv.FormatFloat(dur, 'f', -1, 64)
	cmd := exec.Command("ffmpeg",
		"-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size="+size+":rate=25:duration="+secs,
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+secs,
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "16",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k",
		"-shortest", outpath,
	)
	require.NoError(t, cmd.Run(), "failed to generate test video")
}

// TestShouldTranscodeVideo pins down the decision rule: only videos wider than
// the target get downscaled, everything else is left untouched.
func TestShouldTranscodeVideo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		w, h      int
		wantWidth int
		wantOK    bool
	}{
		{name: "1080p landscape", w: 1920, h: 1080, wantWidth: 640, wantOK: true},
		{name: "720p landscape", w: 1280, h: 720, wantWidth: 640, wantOK: true},
		{name: "just over target", w: 641, h: 360, wantWidth: 640, wantOK: true},
		{name: "exactly target", w: 640, h: 360, wantWidth: 0, wantOK: false},
		{name: "already small", w: 480, h: 270, wantWidth: 0, wantOK: false},
		{name: "portrait phone video", w: 1080, h: 1920, wantWidth: 640, wantOK: true},
		{name: "no dimensions", w: 0, h: 0, wantWidth: 0, wantOK: false},
		{name: "audio only", w: 0, h: 1080, wantWidth: 0, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			width, ok := shouldTranscodeVideo(tc.w, tc.h)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantWidth, width)
		})
	}
}

// TestTranscodeVideoLandscape checks the core promise: a 720p source comes out
// 640 wide, smaller, playable, same duration, audio intact.
func TestTranscodeVideoLandscape(t *testing.T) {
	requireFfmpeg(t)

	src := tmpMediaPath(t)
	makeTestVideo(t, src, 1280, 720, 2)

	srcStat, err := os.Stat(src)
	require.NoError(t, err)

	out, err := transcodeVideo(context.Background(), src, 640)
	require.NoError(t, err)
	require.Equal(t, transcodePath(src), out, "expected a new file beside the input")

	outStat, err := os.Stat(out)
	require.NoError(t, err)

	assert.Less(t, outStat.Size(), srcStat.Size(), "transcoded file must be smaller")

	info := probeVideo(t, out)
	assert.Equal(t, 640, info.width, "width must be capped at the target")
	assert.Equal(t, 360, info.height, "height must follow the aspect ratio")
	assert.Equal(t, "h264", info.vcodec)
	assert.InDelta(t, 2.0, info.dur, 0.5, "duration must be preserved")
	assert.True(t, info.hasAudio, "audio stream must be preserved")
}

// TestTranscodeVideoPortrait covers phone-native portrait uploads: the width is
// capped and the proportional height is rounded to an even number.
func TestTranscodeVideoPortrait(t *testing.T) {
	requireFfmpeg(t)

	src := tmpMediaPath(t)
	makeTestVideo(t, src, 1080, 1920, 1)

	out, err := transcodeVideo(context.Background(), src, 640)
	require.NoError(t, err)

	info := probeVideo(t, out)
	assert.Equal(t, 640, info.width)
	assert.Zero(t, info.height%2, "height must be even for yuv420p")
	assert.InDelta(t, 1080.0/1920.0, float64(info.width)/float64(info.height), 0.01,
		"aspect ratio must be kept")
}

// TestTranscodeVideoNotNeeded ensures we never waste a transcode (or upscale) a
// file that is already within the limit.
func TestTranscodeVideoNotNeeded(t *testing.T) {
	requireFfmpeg(t)

	src := tmpMediaPath(t)
	makeTestVideo(t, src, 480, 270, 1)

	out, err := transcodeVideo(context.Background(), src, 640)
	assert.ErrorIs(t, err, ErrTranscodeNotNeeded)
	assert.Empty(t, out)

	_, statErr := os.Stat(transcodePath(src))
	assert.True(t, os.IsNotExist(statErr), "no output file may be left behind")
}

// TestTranscodeVideoBrokenInput ensures a failure is reported and leaves no
// half-written artifact behind.
func TestTranscodeVideoBrokenInput(t *testing.T) {
	requireFfmpeg(t)

	src := tmpMediaPath(t)
	require.NoError(t, os.WriteFile(src, []byte("definitely not a video"), 0o600))

	out, err := transcodeVideo(context.Background(), src, 640)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrTranscodeNotNeeded)
	assert.Empty(t, out)

	_, statErr := os.Stat(transcodePath(src))
	assert.True(t, os.IsNotExist(statErr), "failed transcode must not leave artifacts")
}

// TestTranscodeVideoNoAudio makes sure a silent clip does not error out on a
// missing audio stream.
func TestTranscodeVideoNoAudio(t *testing.T) {
	requireFfmpeg(t)

	src := tmpMediaPath(t)
	cmd := exec.Command("ffmpeg",
		"-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "16",
		"-pix_fmt", "yuv420p", "-an", src,
	)
	require.NoError(t, cmd.Run())

	out, err := transcodeVideo(context.Background(), src, 640)
	require.NoError(t, err)

	info := probeVideo(t, out)
	assert.Equal(t, 640, info.width)
	assert.Equal(t, 360, info.height)
	assert.False(t, info.hasAudio, "silent input stays silent")
}
