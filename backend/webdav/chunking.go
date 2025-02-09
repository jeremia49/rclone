package webdav

/*
	chunked update for Nextcloud
	see https://docs.nextcloud.com/server/20/developer_manual/client_apis/WebDAV/chunking.html
*/

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/readers"
	"github.com/rclone/rclone/lib/rest"
)

func (f *Fs) shouldRetryChunkMerge(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// Not found. Can be returned by NextCloud when merging chunks of an upload.
	if resp != nil && resp.StatusCode == 404 {
		return true, err
	}

	// 423 LOCKED
	if resp != nil && resp.StatusCode == 423 {
		return false, fmt.Errorf("merging the uploaded chunks failed with 423 LOCKED. This usually happens when the chunks merging is still in progress on NextCloud, but it may also indicate a failed transfer: %w", err)
	}

	return f.shouldRetry(ctx, resp, err)
}

// set the chunk size for testing
func (f *Fs) setUploadChunkSize(cs fs.SizeSuffix) (old fs.SizeSuffix, err error) {
	old, f.opt.ChunkSize = f.opt.ChunkSize, cs
	return
}

func (f *Fs) getChunksUploadURL() string {
	return strings.Replace(f.endpointURL, "/dav/files/", "/dav/uploads/", 1)
}

func (o *Object) getChunksUploadDir() (string, error) {
	hasher := md5.New()
	_, err := hasher.Write([]byte(o.filePath()))
	if err != nil {
		return "", fmt.Errorf("chunked upload couldn't hash URL: %w", err)
	}
	uploadDir := "rclone-chunked-upload-" + hex.EncodeToString(hasher.Sum(nil))
	return uploadDir, nil
}

func (f *Fs) verifyChunkConfig() error {
	if f.opt.ChunkSize != 0 && !validateNextCloudChunkedURL.MatchString(f.endpointURL) {
		return errors.New("chunked upload with nextcloud must use /dav/files/USER endpoint not /webdav")
	}

	return nil
}

func (o *Object) shouldUseChunkedUpload(src fs.ObjectInfo) bool {
	return o.fs.canChunk && o.fs.opt.ChunkSize > 0 && src.Size() > int64(o.fs.opt.ChunkSize)
}

func (o *Object) updateChunked(ctx context.Context, in0 io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	var uploadDir string

	contentType := fs.MimeType(ctx, src)
	_ = o.filePath()
	extraHeaders := o.extraHeaders(ctx, src)

	// see https://docs.nextcloud.com/server/24/developer_manual/client_apis/WebDAV/chunking.html#starting-a-chunked-upload
	uploadDir, err = o.createChunksUploadDirectory(ctx)
	if err != nil {
		return err
	}

	partObj := &Object{
		fs: o.fs,
	}
	// err = o.updateSimple(ctx, in, nil, filePath, src.Size(), contentType, extraHeaders, o.fs.endpointURL, options...)
	// see https://docs.nextcloud.com/server/24/developer_manual/client_apis/WebDAV/chunking.html#uploading-chunks
	err = o.uploadChunks(ctx, in0, src.Size(), contentType, extraHeaders, partObj, uploadDir, options)
	if err != nil {
		return err
	}

	// see https://docs.nextcloud.com/server/24/developer_manual/client_apis/WebDAV/chunking.html#assembling-the-chunks
	err = o.mergeChunks(ctx, uploadDir, options, src)
	if err != nil {
		return err
	}

	return nil
}

func (o *Object) uploadChunks(ctx context.Context, in0 io.Reader, size int64, contentType string, extraHeaders map[string]string, partObj *Object, uploadDir string, options []fs.OpenOption) error {
	chunkSize := int64(partObj.fs.opt.ChunkSize)

	// var wg sync.WaitGroup
	// TODO: upload chunks in parallel for faster transfer speeds
	for offset := int64(0); offset < size; offset += chunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		fs.Debugf(o, "Offset : "+strconv.Itoa(int(offset)))
		// wg.Add(1)
		func(offset int64) {
			contentLength := chunkSize

			// Last chunk may be smaller
			if size-offset < contentLength {
				contentLength = size - offset
			}

			_ = offset + contentLength - 1

			partObj.remote = fmt.Sprintf("%s/%d", uploadDir, offset)
			// Enable low-level HTTP 2 retries.
			// 2022-04-28 15:59:06 ERROR : stuff/video.avi: Failed to copy: uploading chunk failed: Put "https://censored.com/remote.php/dav/uploads/Admin/rclone-chunked-upload-censored/000006113198080-000006123683840": http2: Transport: cannot retry err [http2: Transport received Server's graceful shutdown GOAWAY] after Request.Body was written; define Request.GetBody to avoid this error

			buf := make([]byte, chunkSize)
			in := readers.NewRepeatableLimitReaderBuffer(in0, buf, chunkSize)

			getBody := func() (io.ReadCloser, error) {
				// RepeatableReader{} plays well with accounting so rewinding doesn't make the progress buggy
				if _, err := in.Seek(0, io.SeekStart); err == nil {
					return nil, err
				}

				return io.NopCloser(in), nil
			}

			err := partObj.updateSimple(ctx, in, getBody, partObj.remote, contentLength, contentType, nil, o.fs.chunksUploadURL, options...)
			if err != nil {
				fs.Errorf(ctx, "uploading chunk failed: %w", err)
			}

			// defer wg.Done()

		}(offset)

	}
	// wg.Wait()

	return nil
}

func (o *Object) createChunksUploadDirectory(ctx context.Context) (string, error) {
	uploadDir, err := o.getChunksUploadDir()
	if err != nil {
		return uploadDir, err
	}

	err = o.purgeUploadedChunks(ctx, uploadDir)
	if err != nil {
		return "", fmt.Errorf("chunked upload couldn't purge upload directory: %w", err)
	}

	opts := rest.Opts{
		Method:     "MKCOL",
		Path:       uploadDir + "/",
		NoResponse: true,
		RootURL:    o.fs.chunksUploadURL,
	}
	err = o.fs.pacer.CallNoRetry(func() (bool, error) {
		resp, err := o.fs.srv.Call(ctx, &opts)
		return o.fs.shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return "", fmt.Errorf("making upload directory failed: %w", err)
	}
	return uploadDir, err
}

func (o *Object) mergeChunks(ctx context.Context, uploadDir string, options []fs.OpenOption, src fs.ObjectInfo) error {
	var resp *http.Response

	// see https://docs.nextcloud.com/server/24/developer_manual/client_apis/WebDAV/chunking.html?highlight=chunk#assembling-the-chunks
	opts := rest.Opts{
		Method:     "MOVE",
		Path:       path.Join(uploadDir, ".file"),
		NoResponse: true,
		Options:    options,
		RootURL:    o.fs.chunksUploadURL,
	}
	destinationURL, err := rest.URLJoin(o.fs.endpoint, o.filePath())
	if err != nil {
		return fmt.Errorf("finalize chunked upload couldn't join URL: %w", err)
	}
	opts.ExtraHeaders = o.extraHeaders(ctx, src)
	opts.ExtraHeaders["Destination"] = destinationURL.String()
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err = o.fs.srv.Call(ctx, &opts)
		return o.fs.shouldRetryChunkMerge(ctx, resp, err)
	})
	if err != nil {
		return fmt.Errorf("finalize chunked upload failed, destinationURL: \"%s\": %w", destinationURL, err)
	}
	return err
}

func (o *Object) purgeUploadedChunks(ctx context.Context, uploadDir string) error {
	// clean the upload directory if it exists (this means that a previous try didn't clean up properly).
	opts := rest.Opts{
		Method:     "DELETE",
		Path:       uploadDir + "/",
		NoResponse: true,
		RootURL:    o.fs.chunksUploadURL,
	}

	err := o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.CallXML(ctx, &opts, nil, nil)

		// directory doesn't exist, no need to purge
		if resp.StatusCode == http.StatusNotFound {
			return false, nil
		}

		return o.fs.shouldRetry(ctx, resp, err)
	})

	return err
}
