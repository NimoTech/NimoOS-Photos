package v1

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	commonUpload "github.com/NimoTech/NimoOS-Common/upload"
	"github.com/NimoTech/NimoOS-Common/utils/logger"
	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"
	"go.uber.org/zap"
)

// relativeLocationWriter wraps http.ResponseWriter to rewrite tusd's
// absolute Location header into a relative path. tusd constructs Location
// from the Host header of the request it sees, which after Gateway → Photos
// proxying is "127.0.0.1" (the internal hop). The browser-facing URL is
// different. TUS 1.0 allows the Location header to be a path-only URL, which
// the client resolves against the original endpoint origin.
type relativeLocationWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *relativeLocationWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		if loc := w.Header().Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil && u.Path != "" {
				w.Header().Set("Location", u.Path)
			}
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *relativeLocationWriter) Write(b []byte) (int, error) {
	// Some handlers write body before explicitly calling WriteHeader; ensure
	// our header rewrite still runs.
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets tusd v2 reach the original ResponseWriter's SetReadDeadline /
// SetWriteDeadline and other control APIs via http.NewResponseController.
// Otherwise the logs get spammed with "NetworkControlError / feature not
// supported" warnings (harmless to upload success, but it falls back to a
// fixed timeout instead of tusd's dynamic timeout).
func (w *relativeLocationWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// withRelativeLocation wraps an http.Handler so that any absolute Location
// header it sets is rewritten to a path-only URL.
func withRelativeLocation(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&relativeLocationWriter{ResponseWriter: w}, r)
	})
}

// freeBytesFn returns available bytes on /DATA. Injectable for tests.
type freeBytesFn func() (uint64, error)

// statfsDATA returns available bytes on /DATA via syscall.Statfs.
func statfsDATA() (uint64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs("/DATA", &s); err != nil {
		return 0, err
	}
	// Bavail = blocks available to unprivileged user
	return s.Bavail * uint64(s.Bsize), nil
}

// checkQuota returns an error if uploadLength (with 5% margin) would not fit.
// available is the free-bytes provider (statfsDATA in prod, mock in tests).
func checkQuota(uploadLength int64, available freeBytesFn) error {
	avail, err := available()
	if err != nil {
		return fmt.Errorf("storage check failed: %w", err)
	}
	needed := uint64(float64(uploadLength) * 1.05)
	if needed > avail {
		return fmt.Errorf("insufficient storage: need %d available %d", needed, avail)
	}
	return nil
}

// validateMetadataWithQuota checks metadata and quota with an injectable free-bytes provider.
func validateMetadataWithQuota(hook handler.HookEvent, quota freeBytesFn) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	meta := hook.Upload.MetaData
	name := strings.TrimSpace(meta["filename"])
	if name == "" {
		return handler.HTTPResponse{}, handler.FileInfoChanges{}, fmt.Errorf("filename metadata required")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return handler.HTTPResponse{}, handler.FileInfoChanges{}, fmt.Errorf("filename contains illegal characters")
	}
	if hook.Upload.Size <= 0 {
		return handler.HTTPResponse{}, handler.FileInfoChanges{}, fmt.Errorf("empty file rejected")
	}
	if hook.Upload.Size > common.MaxUploadSize {
		return handler.HTTPResponse{}, handler.FileInfoChanges{}, fmt.Errorf("file exceeds %d byte limit", common.MaxUploadSize)
	}
	if err := checkQuota(hook.Upload.Size, quota); err != nil {
		return handler.HTTPResponse{StatusCode: 413}, handler.FileInfoChanges{}, err
	}
	return handler.HTTPResponse{}, handler.FileInfoChanges{}, nil
}

// validateMetadata is the production entry point used by tusd.
func validateMetadata(hook handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	return validateMetadataWithQuota(hook, statfsDATA)
}

// ingestStagedFile moves a completed TUS upload from staging to the gallery
// and enqueues it for indexing. albumID may be empty.
//
// reserve, setPendingAlbum, and submit are injected callbacks that implement
// a four-step enqueue protocol:
//  1. reserve(dest, batchID, batchTotal) — pre-occupies the indexer's seen map
//     and records batch metadata BEFORE the rename. This prevents the fsnotify
//     watcher from racing ahead with a plain Enqueue call (no batchID) the
//     moment the file appears in the gallery directory.
//  2. setPendingAlbum(dest, albumID) — registers the album to join AFTER
//     reserve and BEFORE submit, so the worker cannot pick the item up before
//     the album entry is stored.
//  3. rename/copy the staged file into the gallery directory.
//  4. submit(dest, batchID) — pushes the item into the worker queue AFTER the
//     rename has succeeded.
func ingestStagedFile(
	stagedPath string,
	filename string,
	albumID string,
	batchID string,
	batchTotal int64,
	reserve func(path, batchID string, batchTotal int64) bool,
	submit func(path, batchID string),
	setPendingAlbum func(path, albumID string),
	galleryDir string,
) error {
	dest := filepath.Join(galleryDir, filename)

	// Step 1: pre-occupy seen + record batch metadata before rename.
	if !reserve(dest, batchID, batchTotal) {
		return fmt.Errorf("path already pending: %s", dest)
	}

	// Step 2: register album membership BEFORE submit so the worker cannot
	// start processing the file before the pending entry is stored.
	if albumID != "" {
		setPendingAlbum(dest, albumID)
	}

	// Step 3: rename / copy into gallery.
	if err := os.Rename(stagedPath, dest); err != nil {
		// Fallback: copy + delete (cross-fs case)
		if cerr := copyFile(stagedPath, dest); cerr != nil {
			return fmt.Errorf("rename and copy both failed: %w / %v", err, cerr)
		}
		os.Remove(stagedPath) //nolint:errcheck
	}
	// Remove .info sidecar
	os.Remove(stagedPath + ".info") //nolint:errcheck

	// Step 4: push into worker queue.
	submit(dest, batchID)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// NewTUSHandler creates the tusd handler wired to the staging dir,
// metadata validator, upload.Store lifecycle tracking, and complete-hook → Indexer.
// Returns an http.Handler that Echo can wrap via echo.WrapHandler.
func NewTUSHandler(svc service.Services, galleryDir string, uploadStore commonUpload.Store) (http.Handler, error) {
	if err := os.MkdirAll(common.StagingDir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir staging: %w", err)
	}
	fileStore := filestore.New(common.StagingDir)
	composer := handler.NewStoreComposer()
	fileStore.UseIn(composer)

	tusH, err := handler.NewHandler(handler.Config{
		BasePath:                "/v1/upload-tus/",
		StoreComposer:           composer,
		NotifyCompleteUploads:   true,
		NotifyCreatedUploads:    true,
		NotifyUploadProgress:    true,
		NotifyTerminatedUploads: true,
		MaxSize:                 common.MaxUploadSize,
		PreUploadCreateCallback: validateMetadata,
	})
	if err != nil {
		return nil, err
	}

	// Create: write the task row.
	go func() {
		for ev := range tusH.CreatedUploads {
			ownerID := ev.HTTPRequest.Header.Get("X-NimoOS-User-ID")
			ua := ev.HTTPRequest.Header.Get("User-Agent")
			addr := ev.HTTPRequest.RemoteAddr
			task := commonUpload.NewTask(
				ev.Upload.ID,
				ownerID,
				ev.Upload.MetaData,
				ev.Upload.Size,
				ua,
				addr,
				commonUpload.DefaultIdleTimeoutSeconds,
				time.Now(),
			)
			if cerr := uploadStore.Create(task); cerr != nil {
				logger.Error("photos upload task create failed",
					zap.String("id", ev.Upload.ID), zap.Error(cerr))
			}
		}
	}()

	// Progress: update offset and renew expiry.
	go func() {
		for ev := range tusH.UploadProgress {
			_ = uploadStore.UpdateOffset(ev.Upload.ID, ev.Upload.Offset,
				time.Now().Unix()+commonUpload.DefaultIdleTimeoutSeconds)
		}
	}()

	// Termination (protocol DELETE): mark canceled.
	go func() {
		for ev := range tusH.TerminatedUploads {
			_ = uploadStore.SetStatus(ev.Upload.ID, commonUpload.UploadStatusCanceled,
				time.Now().Unix()+commonUpload.DefaultCanceledTTLSeconds)
		}
	}()

	// Complete: ingest (the existing four-step MarkAndReserve/SetPendingAlbum/rename/SubmitReserved)
	// sets completed on success, failed on failure.
	go func() {
		for event := range tusH.CompleteUploads {
			uploadID := event.Upload.ID
			stagedPath := filepath.Join(common.StagingDir, uploadID)
			filename := event.Upload.MetaData["filename"]
			albumID := event.Upload.MetaData["albumId"]
			batchID := event.Upload.MetaData["batch_id"]
			var batchTotal int64
			if bt := event.Upload.MetaData["batch_total"]; bt != "" {
				if n, perr := strconv.ParseInt(bt, 10, 64); perr == nil {
					batchTotal = n
				}
			}
			if ierr := ingestStagedFile(
				stagedPath, filename, albumID,
				batchID, batchTotal,
				svc.Indexer().MarkAndReserve,
				svc.Indexer().SubmitReserved,
				svc.Indexer().SetPendingAlbum,
				galleryDir,
			); ierr != nil {
				zap.L().Error("ingestStagedFile failed",
					zap.String("id", uploadID), zap.Error(ierr))
				_ = uploadStore.SetFailed(uploadID, ierr.Error(),
					time.Now().Unix(),
					time.Now().Unix()+commonUpload.DefaultCanceledTTLSeconds)
				continue
			}
			_ = uploadStore.SetStatus(uploadID, commonUpload.UploadStatusCompleted, 0)
		}
	}()

	// tusd v2.8 internally does `strings.Trim(r.URL.Path, "/")` to route. It
	// expects the prefix to be stripped before it sees the request, otherwise
	// the leftover path is interpreted as an upload ID and POST falls through
	// to the "upload resource" branch which only allows GET/HEAD/PATCH/DELETE.
	// Strip /v1/upload-tus before delegating.
	return withRelativeLocation(http.StripPrefix("/v1/upload-tus", tusH)), nil
}
