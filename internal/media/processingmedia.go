// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package media

import (
	"context"
	"os"

	"codeberg.org/gruf/go-errors/v2"
	errorsv2 "codeberg.org/gruf/go-errors/v2"
	"codeberg.org/gruf/go-runners"

	"code.superseriousbusiness.org/gopkg/log"
	"code.superseriousbusiness.org/gotosocial/internal/config"
	"code.superseriousbusiness.org/gotosocial/internal/gtserror"
	"code.superseriousbusiness.org/gotosocial/internal/gtsmodel"
	"code.superseriousbusiness.org/gotosocial/internal/storage"
	"code.superseriousbusiness.org/gotosocial/internal/uris"
	"code.superseriousbusiness.org/gotosocial/internal/util"
)

// ProcessingMedia represents a piece of media
// currently being processed. It exposes functions
// for retrieving data from the process.
type ProcessingMedia struct {

	// processing media attach details.
	media *gtsmodel.MediaAttachment

	// load data function,
	// returns media stream.
	dataFn DataFunc

	// proc helps synchronize only a
	// singular running processing instance
	proc runners.Processor

	// error stores permanent value when done,
	// or alternatively may store a preset stubError{}
	// value with details to allow skipping processing.
	err error

	// mgr instance, for access to
	// db / storage during processing
	mgr *Manager

	// done is set when process finishes
	// with non ctx canceled type error
	done bool
}

// MustLoad blocks until the thumbnail and fullsize image has been processed, and then returns the completed media.
func (p *ProcessingMedia) Load(ctx context.Context) (*gtsmodel.MediaAttachment, error) {
	media, _, err := p.load(ctx)
	return media, err
}

// MustLoad blocks until the thumbnail and fullsize image has been processed, and then returns the completed
// media. On context cancelled this will enqueue the load for asynchronous operation by dereference worker.
func (p *ProcessingMedia) MustLoad(ctx context.Context) (*gtsmodel.MediaAttachment, error) {
	media, done, err := p.load(ctx)
	if !done {
		// On a context-canceled error (marked as !done), requeue for loading.
		log.Warnf(ctx, "reprocessing media %s after canceled ctx", p.media.ID)
		p.mgr.state.Workers.Dereference.Queue.Push(func(ctx context.Context) {
			if _, _, err := p.load(ctx); err != nil {
				log.Errorf(ctx, "error loading media: %v", err)
			}
		})
	}
	return media, err
}

// Placeholder returns a copy of internally stored processing placeholder,
// returning only the fields that may be known *before* completion, and as
// such all fields which are safe to concurrently read.
func (p *ProcessingMedia) Placeholder() *gtsmodel.MediaAttachment {
	media := new(gtsmodel.MediaAttachment)
	media.ID = p.media.ID
	media.AccountID = p.media.AccountID
	media.StatusID = p.media.StatusID
	media.ScheduledStatusID = p.media.ScheduledStatusID
	media.Description = p.media.Description
	media.Avatar = p.media.Avatar
	media.Header = p.media.Header
	media.RemoteURL = p.media.RemoteURL
	media.Thumbnail.RemoteURL = p.media.Thumbnail.RemoteURL
	media.Blurhash = p.media.Blurhash
	media.FileMeta.Focus.X = p.media.FileMeta.Focus.X
	media.FileMeta.Focus.Y = p.media.FileMeta.Focus.Y
	media.CreatedAt = p.media.CreatedAt

	// We specifically set placeholder URL values that allow an API user to fetch the appropriate
	// media, even if we don't know what filetype it is yet. (since we just parse the IDs from URL path).
	//
	// This way the API caller can (in the worst case that it hasn't loaded yet) attempt to fetch the media,
	// then block on ProcessingMedia{}.Load() for the processing entry it gets from a call to the dereferencer.
	media.Thumbnail.URL = uris.URIForAttachment(media.AccountID, string(TypeAttachment), string(SizeSmall), media.ID, "loading")
	media.URL = uris.URIForAttachment(media.AccountID, string(TypeAttachment), string(SizeOriginal), media.ID, "loading")
	return media
}

// load is the package private form of load() that is wrapped to catch context canceled.
func (p *ProcessingMedia) load(ctx context.Context) (
	media *gtsmodel.MediaAttachment,
	done bool,
	err error,
) {
	err = p.proc.Process(func() (err error) {
		if done = p.done; done {
			// Already proc'd.
			err = p.err
			return
		}

		defer func() {
			// This is only done when ctx NOT cancelled.
			if done = (err == nil || !errorsv2.IsV2(err,
				context.Canceled,
				context.DeadlineExceeded,
			)); done {
				// Processing finished,
				// whether error or not!

				// Anything from here, we
				// need to ensure happens
				// (i.e. no ctx canceled).
				ctx = context.WithoutCancel(ctx)

				// Store values.
				p.done = done
				p.err = err

				// If any error value is stored, including
				// stubError (e.g. unknown type), do cleanup().
				if p.err != nil {
					p.cleanup(ctx)
				}

				// Check the extracted error details on media for
				// stub type error. i.e. policy or media type issue.
				if isStubError(p.media.Error) {
					log.Warnf(ctx, "stubbed %s due to: %v", p.media.RemoteURL, p.err)
					err = nil // don't return stub errors
				}

				// Update with latest details, whatever happened.
				e := p.mgr.state.DB.UpdateAttachment(ctx, p.media)
				if e != nil {
					log.Errorf(ctx, "error updating media in db: %v", e)
				}
			}
		}()

		// If existing error details exists, check if supports retry.
		if withDetails := errors.AsV2[*errWithDetails](p.err); //
		withDetails != nil && !withDetails.details.SupportsRetry() {
			err = p.err
			return
		}

		// Attempt to store media and calculate
		// full-size media attachment details.
		//
		// This will update p.media as it goes.
		err = p.store(ctx)
		return
	})

	// Return a copy of media attachment.
	media = new(gtsmodel.MediaAttachment)
	*media = *p.media
	return
}

// store calls the data function attached to p if it hasn't been called yet,
// and updates the underlying attachment fields as necessary. It will then stream
// bytes from p's reader directly into storage so that it can be retrieved later.
func (p *ProcessingMedia) store(ctx context.Context) error {

	// Load media from data func.
	rc, err := p.dataFn(ctx)
	if err != nil {

		// If a network error, include these details.
		if details := extractNetworkErrorDetails(err); //
		details != 0 {
			err = withDetails(err, details)
		}

		return gtserror.Newf("error executing data function: %w", err)
	}

	var (
		// predefine temporary media
		// file path variables so we
		// can remove them on error.
		temppath  string
		thumbpath string
	)

	defer func() {
		if err := remove(temppath, thumbpath); err != nil {
			log.Errorf(ctx, "error(s) cleaning up files: %v", err)
		}
	}()

	// Drain reader to tmp file
	// (this reader handles close).
	temppath, err = drainToTmp(rc)
	if err != nil {
		return gtserror.Newf("error draining data to tmp: %w", err)
	}

	// Pass input file through ffprobe to
	// parse further metadata information.
	result, err := probe(ctx, temppath)
	if err != nil {
		return gtserror.Newf("ffprobe error: %w", err)
	}

	var ext string

	// Extract any video stream metadata from media.
	// This will always be used regardless of type,
	// as even audio files may contain embedded album art.
	width, height, framerate := result.ImageMeta()
	aspect := util.Div(float32(width), float32(height))
	p.media.FileMeta.Original.Width = width
	p.media.FileMeta.Original.Height = height
	p.media.FileMeta.Original.Size = (width * height)
	p.media.FileMeta.Original.Aspect = aspect
	p.media.FileMeta.Original.Framerate = util.PtrIf(framerate)
	p.media.FileMeta.Original.Duration = util.PtrIf(float32(result.duration))
	p.media.FileMeta.Original.Bitrate = util.PtrIf(result.bitrate)

	// Set generic media type and mimetype from ffprobe format data.
	p.media.Type, p.media.File.ContentType, ext = result.GetFileType()
	if p.media.Type == gtsmodel.FileTypeUnknown {

		// On unsupported return a stub error that doesn't
		// get returned to the caller, but indicates details.
		return withDetails(nil, codecUnsupportedDetails)
	}

	// Add file extension to path.
	newpath := temppath + "." + ext

	// Before ffmpeg processing, rename to set file ext.
	if err := os.Rename(temppath, newpath); err != nil {
		return gtserror.Newf("error renaming to %s - >%s: %w", temppath, newpath, err)
	}

	// Update path var
	// AFTER successful.
	temppath = newpath

	switch p.media.Type {
	case gtsmodel.FileTypeImage,
		gtsmodel.FileTypeGifv:
		// Attempt to clean as much metadata from file as possible.
		if err := clearMetadata(ctx, temppath); err != nil {
			return gtserror.Newf("error cleaning metadata: %w", err)
		}

	case gtsmodel.FileTypeVideo:
		// Downscale oversized video before it is stored. GoToSocial keeps
		// uploads byte-for-byte, so without this a 16Mbps 1080p clip is
		// stored and re-served at over 100MB. See transcode.go.
		targetWidth, transcode := shouldTranscodeVideo(width, height)
		if !transcode {
			// Already small enough: only strip metadata.
			if err := clearMetadata(ctx, temppath); err != nil {
				return gtserror.Newf("error cleaning metadata: %w", err)
			}
			break
		}

		transcoded, err := transcodeVideo(ctx, temppath, targetWidth)
		if err != nil {
			// Never fail the upload over this: a stored-but-large video
			// still beats a lost post. Store the original, cleaned.
			log.Warnf(ctx, "could not transcode video (%dx%d), storing original: %v", width, height, err)
			if err := clearMetadata(ctx, temppath); err != nil {
				return gtserror.Newf("error cleaning metadata: %w", err)
			}
			break
		}

		// Drop the oversize original from tmp storage.
		if err := os.Remove(temppath); err != nil {
			log.Warnf(ctx, "could not remove pre-transcode temp file: %v", err)
		}
		temppath = transcoded

		// The media file now has new dimensions and bitrate, so re-probe
		// it and refresh the metadata that gets stored alongside it.
		result, err = probe(ctx, temppath)
		if err != nil {
			return gtserror.Newf("error probing transcoded video: %w", err)
		}

		width, height, framerate = result.ImageMeta()
		aspect = util.Div(float32(width), float32(height))
		p.media.FileMeta.Original.Width = width
		p.media.FileMeta.Original.Height = height
		p.media.FileMeta.Original.Size = (width * height)
		p.media.FileMeta.Original.Aspect = aspect
		p.media.FileMeta.Original.Framerate = util.PtrIf(framerate)
		p.media.FileMeta.Original.Duration = util.PtrIf(float32(result.duration))
		p.media.FileMeta.Original.Bitrate = util.PtrIf(result.bitrate)

		// Transcodes are always mp4, whatever the input container was, so
		// the stored extension and content type must follow.
		p.media.File.ContentType = "video/mp4"
		ext = "mp4"

	case gtsmodel.FileTypeAudio:
		// NOTE: we do not clean audio file
		// metadata, in order to keep tags.
	}

	if width > 0 && height > 0 {
		// Determine thumbnail dimens to use.
		thumbWidth, thumbHeight := thumbSize(
			config.GetMediaThumbMaxPixels(),
			width,
			height,
			aspect,
		)
		p.media.FileMeta.Small.Width = thumbWidth
		p.media.FileMeta.Small.Height = thumbHeight
		p.media.FileMeta.Small.Size = (thumbWidth * thumbHeight)
		p.media.FileMeta.Small.Aspect = aspect

		// Determine if blurhash needs generating.
		needBlurhash := (p.media.Blurhash == "")
		var newBlurhash, mimeType string

		// Generate thumbnail, and new blurhash if needed from temp media.
		thumbpath, mimeType, newBlurhash, err = generateThumb(ctx, temppath,
			thumbWidth,
			thumbHeight,
			result.orientation,
			result.PixFmt(),
			needBlurhash,
		)
		if err != nil {
			return gtserror.Newf("error generating image thumb: %w", err)
		}

		// Set generated thumbnail's mimetype.
		p.media.Thumbnail.ContentType = mimeType

		if needBlurhash {
			// Set newly determined blurhash.
			p.media.Blurhash = newBlurhash
		}
	}

	// Calculate final media attachment file path.
	p.media.File.Path = uris.StoragePathForAttachment(
		p.media.AccountID,
		string(TypeAttachment),
		string(SizeOriginal),
		p.media.ID,
		ext,
	)

	// Copy temporary file into storage at path.
	filesz, err := p.mgr.state.Storage.PutFile(ctx,
		p.media.File.Path,
		temppath,
		p.media.File.ContentType,
	)
	if err != nil {
		return gtserror.Newf("error writing media to storage: %w", err)
	}

	// Set final determined file size.
	p.media.File.FileSize = int(filesz)

	if thumbpath != "" {
		// Determine final thumbnail ext.
		thumbExt := getExtension(thumbpath)

		// Calculate final media attachment thumbnail path.
		p.media.Thumbnail.Path = uris.StoragePathForAttachment(
			p.media.AccountID,
			string(TypeAttachment),
			string(SizeSmall),
			p.media.ID,
			thumbExt,
		)

		// Copy thumbnail file into storage at path.
		thumbsz, err := p.mgr.state.Storage.PutFile(ctx,
			p.media.Thumbnail.Path,
			thumbpath,
			p.media.Thumbnail.ContentType,
		)
		if err != nil {
			return gtserror.Newf("error writing thumb to storage: %w", err)
		}

		// Set final determined thumbnail size.
		p.media.Thumbnail.FileSize = int(thumbsz)

		// Generate a media attachment thumbnail URL.
		p.media.Thumbnail.URL = uris.URIForAttachment(
			p.media.AccountID,
			string(TypeAttachment),
			string(SizeSmall),
			p.media.ID,
			thumbExt,
		)
	}

	// Generate a media attachment URL.
	p.media.URL = uris.URIForAttachment(
		p.media.AccountID,
		string(TypeAttachment),
		string(SizeOriginal),
		p.media.ID,
		ext,
	)

	// Success! Unset previous
	// error details for media.
	p.media.Error = 0

	return nil
}

// cleanup will remove any traces of processing media from storage.
// and perform any other necessary cleanup steps after failure.
func (p *ProcessingMedia) cleanup(ctx context.Context) {
	if p.media.File.Path != "" {
		// Ensure media file at path is deleted from storage.
		err := p.mgr.state.Storage.Delete(ctx, p.media.File.Path)
		if err != nil && !storage.IsNotFound(err) {
			log.Errorf(ctx, "error deleting %s: %v", p.media.File.Path, err)
		}
	}

	if p.media.Thumbnail.Path != "" {
		// Ensure media thumbnail at path is deleted from storage.
		err := p.mgr.state.Storage.Delete(ctx, p.media.Thumbnail.Path)
		if err != nil && !storage.IsNotFound(err) {
			log.Errorf(ctx, "error deleting %s: %v", p.media.Thumbnail.Path, err)
		}
	}

	// Unset fields.
	p.media.Stub()

	// Extract any error details for db.
	p.media.Error = toErrorDetails(p.err)

	// Also ensure marked as unknown
	// so gets inserted as placeholder URL.
	p.media.Type = gtsmodel.FileTypeUnknown
}
